# Android APK analysis

Notes on how the Marstek Android app was dissected to extract cloud URLs and
the firmware download paths.

## App metadata

| Field | Value |
| --- | --- |
| Package | `com.hamedata.marstek` |
| Display name | MARSTEK |
| Developer | Hamedata Technology Co., Limited |
| Version analysed | 1.6.61 (April 2025 era) |
| Distribution format | XAPK (split APKs bundled in a zip) |

## Tooling

Installed locally via Homebrew:

```bash
brew install jadx            # Java/Kotlin decompiler
# (unzip and strings are stock on macOS)
```

## Unpacking

The XAPK from APKPure is just a zip. The base APK plus per-architecture /
per-language splits fall out of it:

```bash
mkdir apk && cd apk
unzip -o ../MARSTEK_1.6.61_APKPure.xapk
# → com.hamedata.marstek.apk        (base APK, 59 MB)
# → config.arm64_v8a.apk             (native libs, 64 MB)
# → config.<lang>.apk                (resource splits)
```

## Decompiling the Java side (not the payoff)

```bash
jadx -d decoded --no-res --threads-count 8 com.hamedata.marstek.apk
```

This succeeded (32 minor errors out of 6 763 classes) but returned almost
nothing useful when grepped for URLs — the first hits were all references
to the `com.hamedata.marstek.R` resource class.

The giveaway was `io/flutter/embedding/android/FlutterView.java` in the
decompiled tree: this is a **Flutter app**. On Flutter, all the actual
business logic — including any hard-coded URLs — is compiled to native
machine code (Dart AOT snapshot) and stored in `libapp.so`, not in the
Java/Kotlin layer.

## Extracting strings from the Dart snapshot

```bash
cd apk
mkdir arm64 && unzip -o -q config.arm64_v8a.apk -d arm64
strings -n 10 arm64/lib/arm64-v8a/libapp.so > libapp_strings.txt
wc -l libapp_strings.txt   # ~75 000 strings
```

The Dart AOT snapshot embeds every string literal from the source, so:

```bash
grep -iE 'hamedata|hametech' libapp_strings.txt | sort -u
grep -iE '\.bin|\.rbl|/ota/|/download/|firmware' libapp_strings.txt | sort -u
```

…surfaced the full inventory of cloud endpoints (see
[`network.md`](network.md)) and firmware URLs (see
[`firmware.md`](firmware.md)), including the B2500-D download path we were
after.

## Why strings-only was enough

- Dart AOT snapshots don't have a clean DWARF/symbol table, so static analysis
  beyond `strings` is painful.
- For this investigation we only needed to locate the URL base paths — once
  we have the URL pattern we can probe the CDN directly.
- If we ever need richer logic (e.g. to understand the setB2500Report
  encryption), next steps would be:
  - `blutter` (<https://github.com/worawit/blutter>) — reverses Dart AOT
    snapshots into readable pseudo-code if the Flutter/Dart versions match.
  - Frida on a rooted phone to hook Dart method calls at runtime.

## Useful strings retained

The raw extracted dump is small enough to keep around; if we want it in-repo,
dropping the relevant subset into `docs/` is fine but note that re-extraction
from the APK takes seconds, so we don't check the APK itself into git.

Suggested `.gitignore` entries:

```
MARSTEK_*.xapk
apk/
firmware/*.bin
```
