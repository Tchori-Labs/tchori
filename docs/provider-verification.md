# Provider package verification

`tchori providers install` authenticates provider checksum metadata before it
writes a provider binary into the executable cache. This page describes the
verification chain, its trust boundary, and the signing-key formats tchori
accepts.

Source of truth: `internal/registry/registry.go` and its tests.

## Verification chain

The OpenTofu provider registry download descriptor supplies four related
inputs:

| Descriptor field | Role |
| --- | --- |
| `shasums_url` | Locates the `SHA256SUMS` document containing the archive filename and digest. |
| `shasums_signature_url` | Locates the binary detached OpenPGP signature over the exact bytes returned by `shasums_url`. |
| `signing_keys.gpg_public_keys[].ascii_armor` | Supplies the only OpenPGP public keys allowed to verify that signature. |
| `shasum` | Supplies the selected archive's descriptor-level SHA-256 digest for a second metadata cross-check. |

Installation follows this order:

1. Download the selected provider archive to a temporary file while computing
   its SHA-256 digest.
2. Fetch `SHA256SUMS` once and retain its exact bytes.
3. Fetch the binary detached signature and verify it over those exact retained
   bytes against the directly advertised public keys.
4. Find exactly one case-sensitive `SHA256SUMS` entry for the descriptor's
   `filename`.
5. Validate both that entry and the descriptor `shasum` as exactly 64
   hexadecimal characters, normalize both to lowercase, and require them to
   agree.
6. Require the computed archive digest to agree with the authenticated entry.
7. Only after all checks succeed, create the cache directory, extract the
   provider binary, and mark it executable.

Using the same retained bytes for signature verification and checksum lookup is
intentional. Reformatting, reparsing, or refetching the document between those
operations could verify one byte sequence while trusting another.

## Fail-closed policy

Installation stops before cache extraction if any authenticity or integrity
input is missing, ambiguous, or invalid. In particular, tchori rejects:

- a missing `shasums_signature_url` or empty signing-key list;
- empty or malformed `ascii_armor` in any advertised key entry;
- a corrupt, truncated, unverifiable, or unadvertised-key signature;
- a signature that does not cover the exact downloaded `SHA256SUMS` bytes;
- a missing or duplicate filename entry, including duplicate entries with the
  same digest;
- a digest that is not exactly 64 hexadecimal characters;
- disagreement between the signed entry and descriptor `shasum`; and
- disagreement between the signed entry and the downloaded archive.

Uppercase and lowercase hexadecimal digests are accepted only after strict
hexadecimal validation and are compared in canonical lowercase form. Filenames
remain case-sensitive.

Provider archives remain temporary until verification finishes. Verification
failures remove the temporary archive and do not create a provider cache path
or executable. Registry, archive, checksum, and signature requests all use the
caller's context, so cancellation interrupts the network operation and retains
normal Go context error semantics.

## Trust model

The signature establishes that `SHA256SUMS` was signed by a key in the same
registry download descriptor. It prevents an archive host or intermediary that
can alter only the archive or checksum document from substituting a package.
The descriptor digest cross-check also prevents inconsistent registry metadata
from selecting a different signed digest.

The registry remains the trust root because it advertises the allowed keys. A
registry that is itself compromised can replace the key, signature, checksum,
and archive together. tchori does not currently pin provider signing keys to a
separate local or transparency-backed trust source.

## Supported signing keys

Supported:

- OpenPGP public keys directly present in
  `signing_keys.gpg_public_keys[].ascii_armor`;
- one or more advertised keys, with the detached signature accepted only when
  one of those keys verifies it; and
- binary detached OpenPGP signatures at `shasums_signature_url`, as required by
  the OpenTofu provider registry protocol.

Unsupported:

- `trust_signature` keychain delegation used by some Terraform registry trust
  flows; tchori parses this field for protocol compatibility but does not
  follow or honor it;
- keys discovered from local GPG keyrings, keyservers, embedded certificates,
  or any source other than the current descriptor's `ascii_armor`; and
- ASCII-armored detached signatures in place of the protocol's binary
  detached signature.

Unsupported variants fail verification rather than downgrading to unsigned
SHA-256 checking.
