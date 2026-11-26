"""TLS certificate storage and validation for the Marstek bridge.

The device stores a single PEM certificate chain plus its matching private
key under ``/tls``.  A controller (for example a cert-manager CronJob) POSTs
new PEM material to ``/api/tls``; this module authenticates the request,
validates the PEM, persists the pair as one file, and builds the ``SSLContext``
that ``main.py`` hands to microdot.

MicroPython note: ``ssl.SSLContext.load_cert_chain()`` takes the PEM *contents*
(bytes), not filenames as on CPython.  Keep that call here so the rest of the
firmware never has to know the difference.

Pure parsing helpers are deliberately free of device-only imports so they can
be exercised with CPython's unittest (see ``test_tls_store.py``).
"""
import binascii
import hashlib
import json
import os
import time

TLS_DIR = "tls"
PAIR_PATH = TLS_DIR + "/pair.pem"
META_PATH = TLS_DIR + "/meta.json"

# Enough for a leaf + two intermediates and a 4096-bit RSA key, while keeping
# a single request comfortably under the microdot body limit.
MAX_CERT_BYTES = 12 * 1024
MAX_KEY_BYTES = 8 * 1024
MAX_PAIR_BYTES = MAX_CERT_BYTES + MAX_KEY_BYTES + 2

REBOOT_DELAY_S = 3

# Bearer tokens shorter than this are refused at boot rather than silently
# exposing the endpoint with a guessable secret.
TOKEN_MIN_LEN = 16

_FAIL_WINDOW_MS = 60 * 1000
_MAX_FAILURES = 10

_CERT_HEADER = "-----BEGIN CERTIFICATE-----"
_CERT_FOOTER = "-----END CERTIFICATE-----"
_KEY_PAIRS = (
    ("-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----"),
    ("-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----"),
    ("-----BEGIN EC PRIVATE KEY-----", "-----END EC PRIVATE KEY-----"),
)
_CN_OID = b"\x55\x04\x03"  # 2.5.4.3

_enabled = False
_token = b""
_token_error = None
_context = None
_context_attempted = False
_context_error = None
_meta_cache = None
_reboot_requested = False
_tls_active = False
_pair_healthy = False
_failures = []


# ---------------------------------------------------------------------------
# Configuration and authentication
# ---------------------------------------------------------------------------

def init(cfg: dict) -> None:
    """Apply ``config.json`` values. Called once before the HTTP server starts."""
    global _enabled, _token, _token_error, _context, _context_attempted
    global _context_error, _meta_cache, _reboot_requested, _tls_active
    global _pair_healthy, _failures

    enabled = cfg.get("tls_enabled", False)
    if isinstance(enabled, str):
        enabled = enabled.strip().lower() in ("1", "true", "yes", "on")
    elif not isinstance(enabled, bool):
        enabled = False
    _enabled = bool(enabled)

    token = cfg.get("tls_push_token", "") or ""
    _token_error = None
    if not isinstance(token, str):
        _token_error = "tls_push_token must be a string"
        token = ""
    _token = token.strip().encode()

    _context = None
    _context_attempted = False
    _context_error = None
    _meta_cache = None
    _reboot_requested = False
    _tls_active = False
    _pair_healthy = False
    _failures = []


def tls_enabled() -> bool:
    """Whether HTTPS/certificate provisioning is explicitly enabled."""
    return _enabled


def set_tls_active(active: bool) -> None:
    """Called by main after deciding which listeners actually started."""
    global _tls_active
    _tls_active = bool(active)


def tls_active() -> bool:
    return _tls_active


def set_pair_healthy(healthy: bool) -> None:
    """True when the stored pair built a context and passed the loopback test.

    Cleartext certificate operations stay blocked while this is true even if
    the :443 listener is temporarily down; use HTTPS, wait for the listener
    retry/WDT reset, or recover over USB.
    """
    global _pair_healthy
    _pair_healthy = bool(healthy)


def pair_healthy() -> bool:
    return _pair_healthy


def startup_mode(enabled: bool, installed: bool, context_ok: bool,
                 version_ok: bool, probe_ok: bool) -> str:
    """Decide the listener topology without touching the network.

    ``plain``     - feature disabled; full app on :80.
    ``bootstrap`` - enabled but no cert yet; full app + cert push on :80.
    ``admin``     - cert material exists but TLS is not usable; cert-only :80.
    ``tls``       - HTTPS on :443 + cert-only admin on :80.
    """
    if not enabled:
        return "plain"
    if not installed:
        return "bootstrap"
    # The loopback handshake is the authoritative capability check; the
    # version tuple only explains the failure in logs.
    if not (context_ok and probe_ok):
        return "admin"
    return "tls"


def push_enabled() -> bool:
    return _enabled and not _token_error and len(_token) >= TOKEN_MIN_LEN


def token_problem() -> str | None:
    """Human-readable reason the push endpoint is disabled, if it is."""
    if not _enabled:
        return "tls_enabled is false (opt in with \"tls_enabled\": true)"
    if _token_error:
        return _token_error
    if not _token:
        return "tls_push_token is not configured"
    if len(_token) < TOKEN_MIN_LEN:
        return "tls_push_token must be at least " + str(TOKEN_MIN_LEN) + " characters"
    return None


def _ct_eq(a: bytes, b: bytes) -> bool:
    """Constant-time byte comparison (length leak is acceptable for tokens)."""
    if len(a) != len(b):
        return False
    diff = 0
    for i in range(len(a)):
        diff |= a[i] ^ b[i]
    return diff == 0


def _now_ms() -> int:
    try:
        return time.ticks_ms()
    except AttributeError:  # CPython / unittest
        return int(time.time() * 1000)


def _diff_ms(a: int, b: int) -> int:
    try:
        return time.ticks_diff(a, b)
    except AttributeError:  # CPython / unittest
        return a - b


def _prune_failures(now: int) -> None:
    global _failures
    _failures = [t for t in _failures if _diff_ms(now, t) < _FAIL_WINDOW_MS]


def _rate_limited() -> bool:
    now = _now_ms()
    _prune_failures(now)
    return len(_failures) >= _MAX_FAILURES


def _record_failure() -> None:
    _prune_failures(_now_ms())
    _failures.append(_now_ms())


def auth_status(req) -> str | None:
    """Return None when authorized, else ``disabled``/``rate_limited``/``unauthorized``.

    ``req`` only needs a ``headers`` mapping, which keeps this testable without
    pulling microdot into the module.
    """
    if not push_enabled():
        return "disabled"

    # Validate first: a valid token must never be denied by a failure budget
    # inflated by other clients. Wrong tokens still consume the same window.
    header = req.headers.get("Authorization", "") or ""
    provided = b""
    if header[:7].lower() == "bearer ":
        provided = header[7:].strip().encode()
    if provided and _ct_eq(provided, _token):
        _failures.clear()
        return None

    if _rate_limited():
        return "rate_limited"
    _record_failure()
    return "unauthorized"


# ---------------------------------------------------------------------------
# PEM validation
# ---------------------------------------------------------------------------

def _as_text(value) -> str:
    if isinstance(value, bytes):
        try:
            return value.decode("utf-8")
        except Exception:
            raise ValueError("PEM data must be UTF-8 text")
    if not isinstance(value, str):
        raise ValueError("PEM data must be a string")
    return value


def _normalize_text(value) -> str:
    text = _as_text(value).replace("\r\n", "\n").replace("\r", "\n")
    if "\x00" in text:
        raise ValueError("PEM data contains NUL bytes")
    return text.strip() + "\n"


def normalize_cert(value) -> str:
    text = _normalize_text(value)
    if _CERT_HEADER not in text or _CERT_FOOTER not in text:
        raise ValueError("certificate must be PEM with BEGIN/END CERTIFICATE")
    for begin, _ in _KEY_PAIRS:
        if begin in text:
            raise ValueError("certificate field contains a private key")
    if len(text.encode()) > MAX_CERT_BYTES:
        raise ValueError("certificate chain is too large")
    return text


def normalize_key(value) -> str:
    text = _normalize_text(value)
    if "BEGIN ENCRYPTED PRIVATE KEY" in text:
        raise ValueError("encrypted private keys are not supported; send an unencrypted PEM key")
    if _CERT_HEADER in text:
        raise ValueError("private_key field contains a certificate")
    for begin, end in _KEY_PAIRS:
        if begin in text:
            if end not in text:
                raise ValueError("private key is missing its END marker")
            if len(text.encode()) > MAX_KEY_BYTES:
                raise ValueError("private key is too large")
            return text
    raise ValueError("private key must be a PEM PRIVATE KEY block")


def _pem_blocks(text: str) -> list:
    """Return the DER bytes of every certificate in a PEM chain."""
    blocks = []
    rest = text
    while True:
        start = rest.find(_CERT_HEADER)
        if start < 0:
            break
        body_start = start + len(_CERT_HEADER)
        end = rest.find(_CERT_FOOTER, body_start)
        if end < 0:
            raise ValueError("certificate is missing its END marker")
        b64 = "".join(rest[body_start:end].split())
        try:
            der = binascii.a2b_base64(b64)
        except Exception:
            raise ValueError("certificate contains invalid base64")
        if not der:
            raise ValueError("certificate body is empty")
        blocks.append(der)
        rest = rest[end + len(_CERT_FOOTER):]
    if not blocks:
        raise ValueError("no certificate block found")
    return blocks


# ---------------------------------------------------------------------------
# Minimal DER reader: certificate validity and subject CN
# ---------------------------------------------------------------------------

def _read_tlv(data: bytes, pos: int):
    if pos + 2 > len(data):
        raise ValueError("truncated DER")
    tag = data[pos]
    first = data[pos + 1]
    pos += 2
    if first & 0x80:
        n = first & 0x7F
        if n == 0 or n > 4 or pos + n > len(data):
            raise ValueError("invalid DER length")
        length = 0
        for _ in range(n):
            length = (length << 8) | data[pos]
            pos += 1
    else:
        length = first
    end = pos + length
    if end > len(data):
        raise ValueError("truncated DER value")
    return tag, data[pos:end], end


def _days_from_civil(year: int, month: int, day: int) -> int:
    """Howard Hinnant's civil-from-days inverse; avoids mktime/time zones."""
    year -= 1 if month <= 2 else 0
    era = (year if year >= 0 else year - 399) // 400
    yoe = year - era * 400
    doy = (153 * (month + (-3 if month > 2 else 9)) + 2) // 5 + day - 1
    doe = yoe * 365 + yoe // 4 - yoe // 100 + doy
    return era * 146097 + doe - 719468


def _epoch(year: int, month: int, day: int, hour: int, minute: int, second: int) -> int:
    return _days_from_civil(year, month, day) * 86400 + hour * 3600 + minute * 60 + second


def _parse_asn1_time(tag: int, raw: bytes) -> int:
    text = raw.decode("ascii")
    if tag == 0x17:  # UTCTime: YYMMDDHHMM[SS]Z
        if len(text) < 11 or not text.endswith("Z"):
            raise ValueError("unsupported UTCTime")
        yy = int(text[0:2])
        year = 2000 + yy if yy < 50 else 1900 + yy
        pos = 2
    elif tag == 0x18:  # GeneralizedTime: YYYYMMDDHHMM[SS]Z
        if len(text) < 13 or not text.endswith("Z"):
            raise ValueError("unsupported GeneralizedTime")
        year = int(text[0:4])
        pos = 4
    else:
        raise ValueError("unsupported certificate time encoding")
    month = int(text[pos:pos + 2])
    day = int(text[pos + 2:pos + 4])
    hour = int(text[pos + 4:pos + 6])
    minute = int(text[pos + 6:pos + 8])
    second = 0
    if len(text) >= pos + 11:
        second = int(text[pos + 8:pos + 10])
    return _epoch(year, month, day, hour, minute, second)


def _find_cn(name_der: bytes) -> str:
    pos = 0
    while pos < len(name_der):
        tag, rdn, pos = _read_tlv(name_der, pos)
        if tag != 0x31:  # SET
            continue
        inner = 0
        while inner < len(rdn):
            t, atv, inner = _read_tlv(rdn, inner)
            if t != 0x30:  # SEQUENCE
                continue
            p = 0
            t_oid, oid, p = _read_tlv(atv, p)
            if t_oid != 0x06 or oid != _CN_OID:
                continue
            t_val, val, p = _read_tlv(atv, p)
            try:
                return val.decode("utf-8")
            except Exception:
                try:
                    return val.decode("ascii")
                except Exception:
                    return ""
    return ""


def parse_certificate(der: bytes) -> dict:
    tag, cert, _ = _read_tlv(der, 0)
    if tag != 0x30:
        raise ValueError("certificate is not a DER SEQUENCE")
    tag, tbs, _ = _read_tlv(cert, 0)
    if tag != 0x30:
        raise ValueError("certificate TBS is not a DER SEQUENCE")

    pos = 0
    tag, _, pos = _read_tlv(tbs, pos)
    if tag == 0xA0:  # optional version; the first read consumed it
        tag, _, pos = _read_tlv(tbs, pos)  # serial
    # signature AlgorithmIdentifier
    tag, _, pos = _read_tlv(tbs, pos)
    # issuer
    tag, _, pos = _read_tlv(tbs, pos)
    # validity
    tag, validity, pos = _read_tlv(tbs, pos)
    if tag != 0x30:
        raise ValueError("certificate validity is not a SEQUENCE")
    tb_tag, tb_raw, vpos = _read_tlv(validity, 0)
    ta_tag, ta_raw, _ = _read_tlv(validity, vpos)
    not_before = _parse_asn1_time(tb_tag, tb_raw)
    not_after = _parse_asn1_time(ta_tag, ta_raw)
    # subject
    tag, subject, pos = _read_tlv(tbs, pos)
    return {
        "common_name": _find_cn(subject) if tag == 0x30 else "",
        "not_before": not_before,
        "not_after": not_after,
    }


def _sha256_hex(data: bytes) -> str:
    # MicroPython's hashlib intentionally omits hexdigest(); binascii is
    # available on both CPython and the ESP32 port.
    return binascii.hexlify(hashlib.sha256(data).digest()).decode()


def cert_info(cert_text) -> dict:
    cert = normalize_cert(cert_text)
    blocks = _pem_blocks(cert)
    info = parse_certificate(blocks[0])
    info["chain_length"] = len(blocks)
    info["fingerprint_sha256"] = _sha256_hex(blocks[0])
    info["valid_for_days"] = (info["not_after"] - info["not_before"]) // 86400
    return info


# ---------------------------------------------------------------------------
# Persistence
# ---------------------------------------------------------------------------

def _ensure_dir() -> None:
    try:
        os.mkdir(TLS_DIR)
    except OSError:
        pass


def _read_text(path: str, limit: int) -> str | None:
    try:
        with open(path, "rb") as f:
            data = f.read(limit + 1)
    except OSError:
        return None
    if not data or len(data) > limit:
        return None
    try:
        return data.decode("utf-8")
    except Exception:
        return None


def _write_atomic(path: str, data: bytes) -> None:
    tmp = path + ".tmp"
    with open(tmp, "wb") as f:
        f.write(data)
    try:
        os.remove(path)
    except OSError:
        pass
    os.rename(tmp, path)


def common_name() -> str:
    """Cached CN from metadata; avoids parsing PEM on every admin request."""
    return str(_read_meta().get("common_name", ""))


def _read_meta() -> dict:
    global _meta_cache
    if _meta_cache is not None:
        return _meta_cache
    raw = _read_text(META_PATH, 1024)
    if raw is None:
        _meta_cache = {}
        return _meta_cache
    try:
        meta = json.loads(raw)
    except Exception:
        meta = {}
    _meta_cache = meta if isinstance(meta, dict) else {}
    return _meta_cache


def _path_present(path: str) -> bool:
    try:
        os.stat(path)
        return True
    except OSError:
        return False


def _read_pair_text():
    """Return ``(text_or_None, present)``.

    A present-but-unreadable pair must not be mistaken for "no certificate":
    that would turn an opt-in deployment into the full bootstrap HTTP app.
    """
    for path in (PAIR_PATH, PAIR_PATH + ".tmp"):
        if _path_present(path):
            return _read_text(path, MAX_PAIR_BYTES), True
    return None, False


def _split_pair(text: str) -> tuple:
    """Split the single stored blob into normalized (cert_pem, key_pem)."""
    for begin, _ in _KEY_PAIRS:
        idx = text.find(begin)
        if idx >= 0:
            cert = text[:idx].strip()
            key = text[idx:].strip()
            if not cert or not key:
                raise ValueError("stored TLS pair is incomplete")
            return normalize_cert(cert), normalize_key(key)
    raise ValueError("stored TLS pair has no private key")


def load() -> tuple | None:
    """Return ``(cert_pem, key_pem)`` or None when nothing valid is installed."""
    raw, _present = _read_pair_text()
    if raw is None:
        return None
    try:
        return _split_pair(raw)
    except ValueError:
        return None


def save(cert_value, key_value, force: bool = False) -> dict:
    """Validate and persist a new certificate. Raises ValueError on bad input."""
    global _context, _context_attempted, _context_error, _meta_cache

    if not _enabled:
        raise ValueError("TLS is disabled in config")
    cert = normalize_cert(cert_value)
    key = normalize_key(key_value)
    info = cert_info(cert)  # also verifies the DER structure before any write

    fingerprint = info["fingerprint_sha256"]
    pair_text = cert + key
    pair_hash = _sha256_hex(pair_text.encode())
    previous = _read_meta()
    stored = load()
    if not force and stored is not None:
        stored_hash = _sha256_hex((stored[0] + stored[1]).encode())
        if stored_hash == pair_hash:
            return {"changed": False, "fingerprint_sha256": fingerprint, "info": info}

    # Prove that mbedTLS can build a server context from these exact bytes
    # before replacing a working certificate.
    _build_context(cert, key)

    _ensure_dir()
    _write_atomic(PAIR_PATH, pair_text.encode())

    generation = int(previous.get("generation", 0)) + 1
    meta = {
        "fingerprint": fingerprint,
        "pair_sha256": pair_hash,
        "generation": generation,
        "common_name": info.get("common_name", ""),
        "not_after": info["not_after"],
    }
    try:
        _write_atomic(META_PATH, json.dumps(meta).encode())
    except Exception:
        # The pair is already committed and is the source of truth. A lost
        # meta write only loses generation/CN hints, not the certificate.
        pass
    _meta_cache = meta
    _context = None
    _context_attempted = False
    _context_error = None
    return {"changed": True, "fingerprint_sha256": fingerprint, "info": info}


def installed_info() -> dict:
    raw, present = _read_pair_text()
    if not present:
        return {"installed": False}
    if raw is None:
        return {
            "installed": True,
            "valid": False,
            "error": "stored pair is unreadable or oversized",
        }
    try:
        cert, key = _split_pair(raw)
        info = cert_info(cert)
        generation = int(_read_meta().get("generation", 0))
    except Exception as exc:
        return {
            "installed": True,
            "valid": False,
            "error": str(exc),
            "pair_bytes": len(raw),
        }
    return {
        "installed": True,
        "valid": True,
        "fingerprint_sha256": info["fingerprint_sha256"],
        "common_name": info.get("common_name", ""),
        "not_before": info["not_before"],
        "not_after": info["not_after"],
        "valid_for_days": info["valid_for_days"],
        "chain_length": info["chain_length"],
        "cert_bytes": len(cert),
        "key_bytes": len(key),
        "generation": generation,
    }


# ---------------------------------------------------------------------------
# SSL context
# ---------------------------------------------------------------------------

def _build_context(cert: str, key: str):
    """Build a server SSLContext from PEM contents (MicroPython signature).

    Imported lazily so a firmware build without the ssl module can still run
    the plain-HTTP bridge; a push then fails cleanly instead of killing import.
    """
    import ssl
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(cert.encode(), key.encode())
    return ctx


def reset_context() -> None:
    """Clear a cached context/failure so a later retry can re-read the pair."""
    global _context, _context_attempted, _context_error
    _context = None
    _context_attempted = False
    _context_error = None


def server_context():
    """Return a cached TLS server context, or None if TLS is off/not usable."""
    global _context, _context_attempted, _context_error
    if not _enabled:
        return None
    if _context is not None:
        return _context
    if _context_attempted:
        return None
    _context_attempted = True
    loaded = load()
    if loaded is None:
        return None
    try:
        _context = _build_context(loaded[0], loaded[1])
        _context_error = None
        return _context
    except Exception as exc:
        _context_error = str(exc)
        return None


def context_error() -> str | None:
    return _context_error


# ---------------------------------------------------------------------------
# Apply / reboot coordination
# ---------------------------------------------------------------------------

def request_reboot() -> None:
    global _reboot_requested
    _reboot_requested = True


def reboot_pending() -> bool:
    return _reboot_requested
