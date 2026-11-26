#!/usr/bin/env python3
"""Push a PEM certificate chain + private key to the Marstek ESP32 bridge.

Intended for a cert-manager CronJob (or any scheduler). Only the Python
standard library is required, so it runs unchanged in a slim container.

Examples:
    # Bootstrap while the device still serves plain HTTP on :80
    python3 scripts/push_esp32_cert.py \
        --url http://esp32.local \
        --cert /certs/tls.crt --key /certs/tls.key \
        --token "$ESP32_TLS_TOKEN"

    # Renewal over HTTPS, verifying the device with the internal CA
    python3 scripts/push_esp32_cert.py \
        --url https://esp32.local --ca /certs/ca.crt \
        --cert /certs/tls.crt --key /certs/tls.key \
        --token "$ESP32_TLS_TOKEN"

    # Inspect what the device currently has installed
    python3 scripts/push_esp32_cert.py --url https://esp32.local \
        --ca /certs/ca.crt --token "$ESP32_TLS_TOKEN" --status

The endpoint is idempotent on the full pair (certificate chain + key):
pushing the same bytes reports ``"changed": false`` and does not reboot.
Use ``--force`` to re-apply identical bytes during recovery.
"""
import argparse
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_RETRIES = 3
DEFAULT_RETRY_DELAY = 5.0
MAX_RESPONSE_BYTES = 64 * 1024


def _endpoint(base_url: str) -> str:
    base = base_url.rstrip("/")
    if base.endswith("/api/tls"):
        return base
    return base + "/api/tls"


def _ssl_context(args):
    if args.insecure:
        context = ssl.create_default_context()
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
        print("warning: TLS verification disabled (--insecure)", file=sys.stderr)
        return context
    if args.ca:
        return ssl.create_default_context(cafile=args.ca)
    return ssl.create_default_context()


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Never forward the bearer token and private key to another origin."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _request(url: str, method: str, token: str, body: bytes | None, context,
             timeout: float) -> tuple:
    headers = {
        "Authorization": "Bearer " + token,
        "User-Agent": "esp32-cert-pusher/1",
    }
    if body is not None:
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(url, data=body, headers=headers, method=method)
    handlers = [_NoRedirect()]
    if urllib.parse.urlparse(url).scheme == "https":
        handlers.append(urllib.request.HTTPSHandler(context=context))
    opener = urllib.request.build_opener(*handlers)
    try:
        with opener.open(request, timeout=timeout) as response:
            payload = response.read(MAX_RESPONSE_BYTES + 1)
            return response.status, payload
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(MAX_RESPONSE_BYTES)


def _decode(payload: bytes):
    try:
        return json.loads(payload.decode("utf-8"))
    except Exception:
        return None


def _fail(message: str, code: int = 1):
    print("error: " + message, file=sys.stderr)
    return code


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        description="Push a TLS certificate to the Marstek ESP32 bridge.")
    parser.add_argument("--url", default=os.environ.get("ESP32_URL", ""),
                        help="Base URL, e.g. http://esp32.local or https://esp32.local "
                             "(env: ESP32_URL)")
    parser.add_argument("--token", default=os.environ.get("ESP32_TLS_TOKEN", ""),
                        help="Bearer token from the device's tls_push_token "
                             "(env: ESP32_TLS_TOKEN)")
    parser.add_argument("--cert", default="", help="PEM certificate chain")
    parser.add_argument("--key", default="", help="PEM private key")
    parser.add_argument("--ca", default=os.environ.get("ESP32_CA_FILE", ""),
                        help="CA bundle used to verify the device (env: ESP32_CA_FILE)")
    parser.add_argument("--insecure", action="store_true",
                        help="Disable TLS verification (bootstrap/self-signed only)")
    parser.add_argument("--status", action="store_true",
                        help="GET and print the installed certificate status")
    parser.add_argument("--force", action="store_true",
                        help="Rewrite/reboot even when the exact pair is already stored "
                             "(use for same-pair recovery)")
    parser.add_argument("--timeout", type=float, default=10.0, help="Request timeout in seconds")
    parser.add_argument("--retries", type=int, default=DEFAULT_RETRIES,
                        help="Attempts for transient failures (default: 3)")
    parser.add_argument("--retry-delay", type=float, default=DEFAULT_RETRY_DELAY,
                        help="Seconds between attempts, doubled each time (default: 5)")
    args = parser.parse_args(argv)

    args.url = args.url.strip()
    args.token = args.token.strip()
    if not args.url:
        return _fail("--url (or ESP32_URL) is required")
    if not args.token:
        return _fail("--token (or ESP32_TLS_TOKEN) is required")
    if len(args.token) < 16:
        return _fail("token is shorter than the device's 16-character minimum")
    if not args.status:
        if not args.cert or not args.key:
            return _fail("--cert and --key are required unless --status is used")
    if args.ca and not args.insecure and not os.path.exists(args.ca):
        return _fail("CA file not found: " + args.ca)

    parsed = urllib.parse.urlparse(args.url)
    if parsed.scheme not in ("http", "https"):
        return _fail("--url must start with http:// or https://")
    if parsed.scheme == "http":
        print("warning: sending credentials over cleartext HTTP", file=sys.stderr)

    url = _endpoint(args.url)
    context = _ssl_context(args)

    body = None
    if not args.status:
        try:
            with open(args.cert, "r", encoding="utf-8") as f:
                certificate = f.read()
            with open(args.key, "r", encoding="utf-8") as f:
                private_key = f.read()
        except OSError as exc:
            return _fail("cannot read certificate/key: " + str(exc))
        payload = {
            "certificate": certificate,
            "private_key": private_key,
        }
        if args.force:
            payload["force"] = True
        body = json.dumps(payload).encode("utf-8")

    method = "GET" if args.status else "POST"
    attempts = max(1, args.retries)
    delay = max(0.0, args.retry_delay)
    last_error = ""
    for attempt in range(1, attempts + 1):
        try:
            status, payload = _request(
                url, method, args.token, body, context, args.timeout)
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            last_error = str(exc)
            status, payload = 0, b""

        decoded = _decode(payload)
        if status == 200 and decoded is not None:
            print(json.dumps(decoded, indent=2, sort_keys=True))
            return 0
        if status == 0:
            last_error = last_error or "connection failed"
        elif decoded is not None and isinstance(decoded, dict):
            last_error = str(decoded.get("error") or decoded)
        else:
            last_error = "HTTP " + str(status)

        # Auth/validation/redirect errors will not improve by retrying.
        if status in (400, 401, 403, 404, 413, 415, 429) or 300 <= status < 400:
            return _fail(last_error, 2)
        if attempt < attempts:
            print("attempt " + str(attempt) + " failed: " + last_error + "; retrying in " +
                  str(delay) + "s", file=sys.stderr)
            time.sleep(delay)
            delay *= 2

    return _fail("giving up after " + str(attempts) + " attempts: " + last_error)


if __name__ == "__main__":
    sys.exit(main())
