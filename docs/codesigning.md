# Code Signing Setup

monolith requires code signing to run on macOS. The `com.apple.security.virtualization`
entitlement is needed to use Apple's Virtualization.framework.

## Quick Start

If you have Xcode installed with an Apple Developer account signed in:

```bash
make build
```

This signs with the `Apple Development` certificate that Xcode manages. If
you've never used Xcode with your Apple ID, open Xcode once and add your
account in **Settings → Accounts**.

## How It Works

The Makefile signs the binary with `com.apple.security.virtualization` using
your Apple Development certificate. This entitlement is required for all
Virtualization.framework APIs (booting VMs, creating virtual network devices).

The `com.apple.vm.networking` entitlement (needed for bridged/host mode
networking via `VZBridgedNetworkDeviceAttachment`) is **not** included because
it requires a provisioning profile from the Apple Developer portal, which
macOS rejects for command-line tools without SIP disabled. NAT, shared, and
isolated network modes work without it.

## Certificate Setup

### Using Xcode (recommended)

1. Open **Xcode → Settings → Accounts**
2. Add your Apple ID if not already added
3. Select your team and click **Manage Certificates**
4. Click **+** → **Apple Development** to create a certificate

The certificate appears in your keychain automatically. Find its name:

```bash
security find-identity -v -p codesigning
```

Example output:

```
1) 32EB20B3... "Apple Development: Your Name (XXXXXXXXXX)"
```

### Updating the Makefile

Set `CODESIGN_IDENTITY` to match your certificate:

```bash
CODESIGN_IDENTITY="Apple Development: Your Name (XXXXXXXXXX)" make build
```

Or edit the default in the `Makefile`:

```makefile
CODESIGN_IDENTITY ?= Apple Development: Your Name (XXXXXXXXXX)
```

### Self-Signed Certificate (fallback)

If you don't have an Apple Developer account, you can create a self-signed
certificate. This works for `make build` **without** entitlements only
(commands like `vm list`, `vm prune`, `vm rm` work, but `vm run` will fail).

```bash
# Generate
cat > /tmp/monolith-cert.cfg <<'EOF'
[ req ]
default_bits       = 2048
distinguished_name = req_dn
x509_extensions    = codesign
prompt             = no

[ req_dn ]
CN = Monolith Dev

[ codesign ]
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, codeSigning
EOF

openssl req -x509 -newkey rsa:2048 \
  -keyout /tmp/monolith-key.pem \
  -out /tmp/monolith-cert.pem \
  -days 3650 -nodes \
  -config /tmp/monolith-cert.cfg

# Export and import
openssl pkcs12 -export \
  -out /tmp/monolith.p12 \
  -inkey /tmp/monolith-key.pem \
  -in /tmp/monolith-cert.pem \
  -passout pass:monolith

security import /tmp/monolith.p12 \
  -k ~/Library/Keychains/login.keychain-db \
  -T /usr/bin/codesign \
  -P "monolith"

security add-trusted-cert -d -r trustRoot -p codeSign \
  -k ~/Library/Keychains/login.keychain-db \
  /tmp/monolith-cert.pem

# Clean up
rm /tmp/monolith-cert.cfg /tmp/monolith-key.pem /tmp/monolith-cert.pem /tmp/monolith.p12
```

Then build without entitlements:

```bash
codesign -f -s "Monolith Dev" ./bin/monolith
```

## Entitlements Reference

| Entitlement | Purpose | Requires Provisioning Profile |
|-------------|---------|-------------------------------|
| `com.apple.security.virtualization` | Virtualization.framework access | No (Apple Development cert is sufficient) |
| `com.apple.vm.networking` | Bridged network (host mode) | Yes (or SIP disabled) |

The default `entitlements.plist` only includes `com.apple.security.virtualization`.

## Network Mode Compatibility

| Network Mode | Entitlement Needed | Works with Default Signing |
|--------------|-------------------|---------------------------|
| `nat` | `com.apple.security.virtualization` | Yes |
| `shared` | `com.apple.security.virtualization` | Yes |
| `isolated` | `com.apple.security.virtualization` | Yes |
| `host` | `com.apple.vm.networking` | No (needs provisioning profile or SIP disabled) |
| `none` | `com.apple.security.virtualization` | Yes |

## Troubleshooting

### `[1] killed bin/monolith vm list`

The binary was signed with a restricted entitlement that macOS rejected.
Check the system log:

```bash
/usr/bin/log show --last 10s --predicate 'eventMessage CONTAINS "monolith"'
```

| Log message | Fix |
|-------------|-----|
| "adhoc signed but contains restricted entitlements" | Sign with an Apple Development certificate |
| "No matching profile found" | Remove `com.apple.vm.networking` from entitlements.plist |

### `0 valid identities found`

No code signing certificates in the keychain. Open Xcode, add your Apple
ID in Settings → Accounts, and create an Apple Development certificate.
