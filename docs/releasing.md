# Releasing

Releases are built by `.github/workflows/release.yml` when a version tag is
pushed. Each release has:

- `monolith_<version>_darwin_arm64.tar.gz` (the binary, `LICENSE`, `README.md`)
- `checksums.txt` (SHA-256)
- a build provenance attestation (`gh attestation verify <archive> --repo bubunyo/monolith`)
- notes generated from the merged PRs

`install.sh` (repo root) installs the latest release for users: it reads the
tag from the GitHub API, downloads the archive and `checksums.txt` from the
release, verifies the checksum and code signature, and installs to
`/usr/local/bin` (`MONOLITH_VERSION`, `MONOLITH_INSTALL_DIR` and
`MONOLITH_BASE_URL` override). It depends on the archive name
`monolith_<version>_darwin_arm64.tar.gz` that `make dist` produces, so keep the
two in step. CI shellchecks it.

## Cutting a release

```bash
git checkout master && git pull
make lint test
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

A tag with a `-` (for example `v0.2.0-rc.1`) is published as a prerelease.

You can also create the release in GitHub's UI (Releases → Draft a new release,
with a new tag). Publishing it creates the tag, which starts the workflow; the
workflow then attaches the archive and checksums to that release and keeps the
title, notes and prerelease setting you chose. The release has no assets for
the few minutes the workflow takes, so `install.sh` fails for that window.
The workflow runs lint and the unit tests again before building, and fails
without publishing if they fail.

## Signing and notarization

Without any secrets the release binary is **ad-hoc signed**. It runs, but a
browser download gets macOS's quarantine attribute and Gatekeeper refuses
it until the user clears it:

```bash
xattr -d com.apple.quarantine monolith
```

To ship a binary that opens without that step, add these repository
secrets (Settings → Secrets and variables → Actions). Signing needs the first
three; notarization needs all six.

| Secret | Value |
|---|---|
| `MACOS_CERTIFICATE` | Your **Developer ID Application** certificate and private key, exported from Keychain Access as `.p12`, base64-encoded: `base64 -i cert.p12 \| pbcopy` |
| `MACOS_CERTIFICATE_PASSWORD` | The password you set when exporting the `.p12` |
| `CODESIGN_IDENTITY` | The identity name, e.g. `Developer ID Application: Your Name (TEAMID)` (`security find-identity -v -p codesigning`) |
| `NOTARY_KEY` | An App Store Connect API key (`.p8`, role Developer), base64-encoded |
| `NOTARY_KEY_ID` | That key's ID |
| `NOTARY_ISSUER` | The issuer ID shown above the keys list in App Store Connect → Users and Access → Integrations |

The workflow imports the certificate into a temporary keychain, signs with
`--options runtime --timestamp` (hardened runtime, required for
notarization), submits the binary to `notarytool` and waits for the result,
then deletes the keychain. An Apple Development certificate (what
`docs/codesigning.md` sets up for local builds) can't be notarized; releases
need a Developer ID Application certificate from a paid Apple Developer
account.

monolith only needs the `com.apple.security.virtualization` entitlement,
which Developer ID binaries may use. Host network mode's
`com.apple.vm.networking` isn't in `entitlements.plist` and isn't available
to release builds.
