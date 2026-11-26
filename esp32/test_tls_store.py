"""CPython tests for tls_store.py.

Storage is exercised with a stubbed context builder; real mbedTLS handshakes
and LittleFS behaviour are device-only. _build_context's byte contract is
checked with a fake ssl module.
"""
import hashlib
import os
import ssl
import sys
import tempfile
import types
import unittest
from unittest import mock

import tls_store

CERT_PEM = """-----BEGIN CERTIFICATE-----
MIIBszCCAVigAwIBAgIUTFnleQ5nU6TzrBsmIR9i6bxm19IwCgYIKoZIzj0EAwIw
IzEhMB8GA1UEAwwYZXNwMzItbWFyc3Rlay10ZXN0LmxvY2FsMB4XDTI2MTEyNjEx
NTk0MFoXDTM2MTEyMzExNTk0MFowIzEhMB8GA1UEAwwYZXNwMzItbWFyc3Rlay10
ZXN0LmxvY2FsMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE3Me+QMUG7FAFU+Yv
YRWaBTnzywD7sHNZfrunaEUc+hj9nS84uHrRh98s2xAyS9hOVtNnKr1AGMubPI6F
/MToz6NqMGgwHQYDVR0OBBYEFOV4Yj0H++6NEAtlnLRXEkwyOTVbMA8GA1UdEwEB
/wQFMAMBAf8wNgYDVR0RBC8wLYIYZXNwMzItbWFyc3Rlay10ZXN0LmxvY2Fsggtl
c3AzMi5sb2NhbIcEwAACCjAKBggqhkjOPQQDAgNJADBGAiEA1pXGZyi98xVj98LO
4RO+IehgNpmBlf13tIhdHqNyFFACIQCnyK1+PNLMeJFIPcep6PvgW2vssLZV2999
5dHMsluKmg==
-----END CERTIFICATE-----
"""

DUMMY_KEY = """-----BEGIN PRIVATE KEY-----
AA==
-----END PRIVATE KEY-----
"""

OTHER_KEY = """-----BEGIN PRIVATE KEY-----
BB==
-----END PRIVATE KEY-----
"""


class FakeRequest:
    def __init__(self, token):
        self.headers = {}
        if token is not None:
            self.headers["Authorization"] = "Bearer " + token


class PemTests(unittest.TestCase):
    def test_normalize_cert_accepts_crlf_and_adds_newline(self):
        normalized = tls_store.normalize_cert(CERT_PEM.replace("\n", "\r\n"))
        self.assertTrue(normalized.endswith("\n"))
        self.assertNotIn("\r", normalized)

    def test_normalize_cert_rejects_garbage(self):
        with self.assertRaises(ValueError):
            tls_store.normalize_cert("not a certificate")

    def test_normalize_cert_rejects_embedded_private_key(self):
        with self.assertRaises(ValueError):
            tls_store.normalize_cert(CERT_PEM + DUMMY_KEY)

    def test_normalize_key_accepts_private_key_block(self):
        normalized = tls_store.normalize_key(DUMMY_KEY)
        self.assertIn("BEGIN PRIVATE KEY", normalized)

    def test_normalize_key_rejects_encrypted_key(self):
        with self.assertRaises(ValueError):
            tls_store.normalize_key(
                "-----BEGIN ENCRYPTED PRIVATE KEY-----\nAA==\n"
                "-----END ENCRYPTED PRIVATE KEY-----\n")

    def test_normalize_key_rejects_certificate(self):
        with self.assertRaises(ValueError):
            tls_store.normalize_key(CERT_PEM)

    def test_normalize_key_requires_end_marker(self):
        with self.assertRaises(ValueError):
            tls_store.normalize_key("-----BEGIN PRIVATE KEY-----\nAA==")

    def test_normalize_key_enforces_size_limit(self):
        oversized = ("-----BEGIN PRIVATE KEY-----\n" +
                     "A" * tls_store.MAX_KEY_BYTES +
                     "\n-----END PRIVATE KEY-----\n")
        with self.assertRaises(ValueError):
            tls_store.normalize_key(oversized)

    def test_normalize_cert_enforces_size_limit(self):
        oversized = CERT_PEM + "A" * tls_store.MAX_CERT_BYTES
        with self.assertRaises(ValueError):
            tls_store.normalize_cert(oversized)

    def test_invalid_base64_certificate_is_rejected(self):
        bad = ("-----BEGIN CERTIFICATE-----\n!!!!\n"
               "-----END CERTIFICATE-----\n")
        with self.assertRaises(ValueError):
            tls_store.cert_info(bad)


class StartupModeTests(unittest.TestCase):
    def test_plain(self):
        self.assertEqual(tls_store.startup_mode(False, False, False, False, False), "plain")

    def test_bootstrap(self):
        self.assertEqual(tls_store.startup_mode(True, False, False, False, False), "bootstrap")

    def test_admin_when_cert_present_but_tls_unusable(self):
        self.assertEqual(tls_store.startup_mode(True, True, True, True, False), "admin")
        self.assertEqual(tls_store.startup_mode(True, True, False, True, True), "admin")
        self.assertEqual(tls_store.startup_mode(True, True, False, False, False), "admin")

    def test_tls_when_everything_works(self):
        self.assertEqual(tls_store.startup_mode(True, True, True, True, True), "tls")
        # The loopback probe is authoritative; a weird version tuple does not
        # override a successful handshake.
        self.assertEqual(tls_store.startup_mode(True, True, True, False, True), "tls")


class FakeSslContext:
    def __init__(self, protocol):
        self.protocol = protocol
        self.cert = None
        self.key = None

    def load_cert_chain(self, cert, key):
        self.cert = cert
        self.key = key


class BuildContextTests(unittest.TestCase):
    def test_build_context_passes_pem_bytes_to_micropython_api(self):
        fake_ssl = types.SimpleNamespace(
            PROTOCOL_TLS_SERVER=1, SSLContext=FakeSslContext)
        with mock.patch.dict(sys.modules, {"ssl": fake_ssl}):
            ctx = tls_store._build_context(CERT_PEM, DUMMY_KEY)
        self.assertIsInstance(ctx.cert, bytes)
        self.assertIsInstance(ctx.key, bytes)
        self.assertIn(b"BEGIN CERTIFICATE", ctx.cert)
        self.assertIn(b"BEGIN PRIVATE KEY", ctx.key)


class CertificateParsingTests(unittest.TestCase):
    def test_sha256_hex_is_available_without_hexdigest(self):
        expected = hashlib.sha256(b"x").digest()

        class DigestOnly:
            def digest(self):
                return expected

        class DigestOnlyHashlib:
            @staticmethod
            def sha256(_data):
                return DigestOnly()

        with mock.patch.object(tls_store, "hashlib", DigestOnlyHashlib):
            self.assertEqual(tls_store._sha256_hex(b"x"),
                             "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881")

    def test_cert_info_matches_openssl_fingerprint_and_dates(self):
        info = tls_store.cert_info(CERT_PEM)
        der = ssl.PEM_cert_to_DER_cert(CERT_PEM)
        self.assertEqual(info["fingerprint_sha256"], hashlib.sha256(der).hexdigest())
        self.assertEqual(info["common_name"], "esp32-marstek-test.local")
        self.assertEqual(info["chain_length"], 1)
        self.assertEqual(info["valid_for_days"], 3650)
        self.assertLess(info["not_before"], info["not_after"])

    def test_chain_length_counts_concatenated_certificates(self):
        info = tls_store.cert_info(CERT_PEM + CERT_PEM)
        self.assertEqual(info["chain_length"], 2)

    def test_civil_date_conversion(self):
        self.assertEqual(tls_store._days_from_civil(1970, 1, 1), 0)
        self.assertEqual(tls_store._epoch(2025, 1, 1, 0, 0, 0), 1735689600)
        self.assertEqual(tls_store._epoch(2024, 2, 29, 12, 0, 0), 1709208000)

    def test_bad_der_is_rejected(self):
        with self.assertRaises(ValueError):
            tls_store.parse_certificate(b"\x30\x00")


class AuthTests(unittest.TestCase):
    TOKEN = "0123456789abcdef-token"

    def setUp(self):
        tls_store.init({"tls_enabled": True, "tls_push_token": self.TOKEN})

    def test_disabled_by_default(self):
        tls_store.init({})
        self.assertFalse(tls_store.tls_enabled())
        self.assertFalse(tls_store.push_enabled())
        self.assertEqual(tls_store.auth_status(FakeRequest(None)), "disabled")
        self.assertIn("tls_enabled", tls_store.token_problem())

    def test_disabled_without_token(self):
        tls_store.init({"tls_enabled": True})
        self.assertTrue(tls_store.tls_enabled())
        self.assertFalse(tls_store.push_enabled())
        self.assertEqual(tls_store.auth_status(FakeRequest(None)), "disabled")
        self.assertIn("not configured", tls_store.token_problem())

    def test_short_token_disables_endpoint(self):
        tls_store.init({"tls_enabled": True, "tls_push_token": "short"})
        self.assertFalse(tls_store.push_enabled())
        self.assertIn("at least", tls_store.token_problem())

    def test_valid_token(self):
        self.assertTrue(tls_store.push_enabled())
        self.assertIsNone(tls_store.auth_status(FakeRequest(self.TOKEN)))

    def test_missing_or_wrong_token(self):
        self.assertEqual(tls_store.auth_status(FakeRequest(None)), "unauthorized")
        self.assertEqual(tls_store.auth_status(FakeRequest("wrong-token")), "unauthorized")

    def test_bearer_scheme_is_case_insensitive(self):
        request = FakeRequest(None)
        request.headers["Authorization"] = "bearer " + self.TOKEN
        self.assertIsNone(tls_store.auth_status(request))

    def test_valid_token_is_not_locked_out_by_failed_attempts(self):
        for _ in range(tls_store._MAX_FAILURES):
            self.assertEqual(tls_store.auth_status(FakeRequest("wrong")), "unauthorized")
        self.assertEqual(tls_store.auth_status(FakeRequest("wrong")), "rate_limited")
        self.assertIsNone(tls_store.auth_status(FakeRequest(self.TOKEN)))

    def test_rate_limit_window_expires(self):
        clock = {"now": 0}
        original = tls_store._now_ms
        tls_store._now_ms = lambda: clock["now"]
        try:
            for _ in range(tls_store._MAX_FAILURES):
                tls_store.auth_status(FakeRequest("wrong"))
            self.assertEqual(tls_store.auth_status(FakeRequest("wrong")), "rate_limited")
            clock["now"] = tls_store._FAIL_WINDOW_MS + 1
            self.assertEqual(tls_store.auth_status(FakeRequest("wrong")), "unauthorized")
        finally:
            tls_store._now_ms = original

    def test_token_minimum_length_boundary(self):
        tls_store.init({"tls_enabled": True, "tls_push_token": "x" * (tls_store.TOKEN_MIN_LEN - 1)})
        self.assertFalse(tls_store.push_enabled())
        tls_store.init({"tls_enabled": True, "tls_push_token": "x" * tls_store.TOKEN_MIN_LEN})
        self.assertTrue(tls_store.push_enabled())

    def test_non_string_token_disables_without_raising(self):
        tls_store.init({"tls_enabled": True, "tls_push_token": 1234567890123456})
        self.assertFalse(tls_store.push_enabled())
        self.assertEqual(tls_store.auth_status(FakeRequest(None)), "disabled")
        self.assertIn("must be a string", tls_store.token_problem())

    def test_successful_auth_resets_failures(self):
        for _ in range(tls_store._MAX_FAILURES - 1):
            tls_store.auth_status(FakeRequest("wrong"))
        self.assertIsNone(tls_store.auth_status(FakeRequest(self.TOKEN)))
        self.assertEqual(tls_store.auth_status(FakeRequest("wrong")), "unauthorized")


class StorageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.old_cwd = os.getcwd()
        os.chdir(self.tmp.name)
        tls_store.init({"tls_enabled": True, "tls_push_token": AuthTests.TOKEN})

    def tearDown(self):
        os.chdir(self.old_cwd)
        self.tmp.cleanup()

    def test_save_rejected_when_tls_disabled(self):
        tls_store.init({})
        with self.assertRaises(ValueError):
            tls_store.save(CERT_PEM, DUMMY_KEY)
        self.assertFalse(os.path.exists(tls_store.PAIR_PATH))

    def test_load_returns_none_when_nothing_installed(self):
        self.assertIsNone(tls_store.load())
        self.assertEqual(tls_store.installed_info(), {"installed": False})

    def test_installed_info_reports_corrupt_file_without_leaking(self):
        os.mkdir(tls_store.TLS_DIR)
        with open(tls_store.PAIR_PATH, "w") as f:
            f.write("broken")
        info = tls_store.installed_info()
        self.assertTrue(info["installed"])
        self.assertFalse(info["valid"])
        self.assertNotIn("private_key", info)
        self.assertNotIn("key_pem", info)

    def test_save_is_idempotent_and_persists_one_pair_file(self):
        original = tls_store._build_context
        tls_store._build_context = lambda cert, key: None
        try:
            first = tls_store.save(CERT_PEM, DUMMY_KEY)
            second = tls_store.save(CERT_PEM, DUMMY_KEY)
        finally:
            tls_store._build_context = original
        self.assertTrue(first["changed"])
        self.assertFalse(second["changed"])
        self.assertEqual(first["fingerprint_sha256"], second["fingerprint_sha256"])
        self.assertTrue(os.path.exists(tls_store.PAIR_PATH))
        loaded = tls_store.load()
        self.assertIsNotNone(loaded)
        self.assertIn("BEGIN CERTIFICATE", loaded[0])
        self.assertIn("BEGIN PRIVATE KEY", loaded[1])
        info = tls_store.installed_info()
        self.assertTrue(info["valid"])
        self.assertEqual(info["common_name"], "esp32-marstek-test.local")
        self.assertEqual(info["generation"], 1)

    def test_force_rewrites_an_identical_pair(self):
        original = tls_store._build_context
        tls_store._build_context = lambda cert, key: None
        try:
            tls_store.save(CERT_PEM, DUMMY_KEY)
            forced = tls_store.save(CERT_PEM, DUMMY_KEY, force=True)
        finally:
            tls_store._build_context = original
        self.assertTrue(forced["changed"])

    def test_same_leaf_with_different_key_is_changed(self):
        original = tls_store._build_context
        tls_store._build_context = lambda cert, key: None
        try:
            first = tls_store.save(CERT_PEM, DUMMY_KEY)
            second = tls_store.save(CERT_PEM, OTHER_KEY)
        finally:
            tls_store._build_context = original
        self.assertTrue(first["changed"])
        self.assertTrue(second["changed"])
        self.assertIn("BB==", tls_store.load()[1])

    def test_changed_chain_with_same_leaf_is_changed(self):
        original = tls_store._build_context
        tls_store._build_context = lambda cert, key: None
        try:
            first = tls_store.save(CERT_PEM, DUMMY_KEY)
            second = tls_store.save(CERT_PEM + CERT_PEM, DUMMY_KEY)
        finally:
            tls_store._build_context = original
        self.assertTrue(first["changed"])
        self.assertTrue(second["changed"])

    def test_rejected_key_does_not_replace_working_pair(self):
        original = tls_store._build_context
        tls_store._build_context = lambda cert, key: None
        try:
            tls_store.save(CERT_PEM, DUMMY_KEY)
        finally:
            tls_store._build_context = original

        with open(tls_store.PAIR_PATH, "r") as f:
            before = f.read()
        with self.assertRaises(ValueError):
            tls_store.save(CERT_PEM, "not a key")
        with open(tls_store.PAIR_PATH, "r") as f:
            self.assertEqual(f.read(), before)

    def test_context_failure_prevents_write(self):
        real_build = tls_store._build_context

        def failing_build(cert, key):
            raise ValueError("invalid key")

        tls_store._build_context = failing_build
        try:
            with self.assertRaises(ValueError):
                tls_store.save(CERT_PEM, DUMMY_KEY)
        finally:
            tls_store._build_context = real_build
        self.assertFalse(os.path.exists(tls_store.PAIR_PATH))

    def test_unexpected_parse_exception_does_not_escape_installed_info(self):
        os.mkdir(tls_store.TLS_DIR)
        with open(tls_store.PAIR_PATH, "w") as f:
            f.write(CERT_PEM + DUMMY_KEY)
        with mock.patch.object(tls_store, "_split_pair", side_effect=RuntimeError("boom")):
            info = tls_store.installed_info()
        self.assertTrue(info["installed"])
        self.assertFalse(info["valid"])
        self.assertIn("boom", info["error"])

    def test_unreadable_pair_is_installed_invalid_not_missing(self):
        os.mkdir(tls_store.TLS_DIR)
        with open(tls_store.PAIR_PATH, "wb") as f:
            f.write(b"\xff\xfe\x00")
        info = tls_store.installed_info()
        self.assertTrue(info["installed"])
        self.assertFalse(info["valid"])
        self.assertIn("unreadable", info["error"])

    def test_load_recovers_completed_tmp_pair(self):
        os.mkdir(tls_store.TLS_DIR)
        with open(tls_store.PAIR_PATH + ".tmp", "w") as f:
            f.write(CERT_PEM + DUMMY_KEY)
        self.assertIsNotNone(tls_store.load())
        self.assertTrue(tls_store.installed_info()["valid"])

    def test_reboot_flags_reset_on_init(self):
        tls_store.request_reboot()
        self.assertTrue(tls_store.reboot_pending())
        tls_store.init({"tls_enabled": True, "tls_push_token": AuthTests.TOKEN})
        self.assertFalse(tls_store.reboot_pending())

    def test_server_context_disabled_returns_none(self):
        tls_store.init({})
        self.assertIsNone(tls_store.server_context())

    def test_server_context_caches_success(self):
        ctx = object()
        with mock.patch.object(tls_store, "load", return_value=(CERT_PEM, DUMMY_KEY)), \
                mock.patch.object(tls_store, "_build_context", return_value=ctx) as build:
            self.assertIs(tls_store.server_context(), ctx)
            self.assertIs(tls_store.server_context(), ctx)
        build.assert_called_once()

    def test_server_context_rebuilds_after_save(self):
        with mock.patch.object(tls_store, "_build_context", return_value=object()) as build:
            tls_store.save(CERT_PEM, DUMMY_KEY)
            self.assertIsNotNone(tls_store.server_context())
            self.assertEqual(build.call_count, 2)
            tls_store.save(CERT_PEM, DUMMY_KEY, force=True)
            self.assertIsNotNone(tls_store.server_context())
        self.assertEqual(build.call_count, 4)

    def test_force_increments_generation(self):
        original = tls_store._build_context
        tls_store._build_context = lambda cert, key: None
        try:
            tls_store.save(CERT_PEM, DUMMY_KEY)
            tls_store.save(CERT_PEM, DUMMY_KEY, force=True)
        finally:
            tls_store._build_context = original
        self.assertEqual(tls_store.installed_info()["generation"], 2)

    def test_server_context_caches_failure(self):
        with mock.patch.object(tls_store, "load", return_value=(CERT_PEM, DUMMY_KEY)), \
                mock.patch.object(tls_store, "_build_context",
                                  side_effect=ValueError("bad key")) as build:
            self.assertIsNone(tls_store.server_context())
            self.assertIn("bad key", tls_store.context_error())
            self.assertIsNone(tls_store.server_context())
        build.assert_called_once()


if __name__ == "__main__":
    unittest.main()
