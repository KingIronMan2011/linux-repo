# linux-repo

Standalone, self-hosted package repository for Debian/Ubuntu (APT), Fedora/RHEL
(DNF/YUM), and Arch Linux (pacman).

It serves native package repositories from one persistent volume and exposes a
token-protected upload API for GitHub Actions. The service generates APT, RPM,
and pacman metadata after every upload.

## API

All publishing calls require `Authorization: Bearer $PUBLISH_TOKEN` and a
multipart form field named `package`.

```sh
# APT, served at https://repo.kingironman.dev/debian/
curl --fail-with-body -X POST https://repo.kingironman.dev/api/v1/packages/debian \
  -H "Authorization: Bearer $PUBLISH_TOKEN" \
  -F package=@my-app_1.0.0_amd64.deb

# DNF, served at https://repo.kingironman.dev/rpm/fedora/41/x86_64/
curl --fail-with-body -X POST 'https://repo.kingironman.dev/api/v1/packages/rpm?release=41&arch=x86_64' \
  -H "Authorization: Bearer $PUBLISH_TOKEN" \
  -F package=@my-app-1.0.0-1.x86_64.rpm

# pacman, served at https://repo.kingironman.dev/arch/x86_64/
curl --fail-with-body -X POST 'https://repo.kingironman.dev/api/v1/packages/arch?arch=x86_64' \
  -H "Authorization: Bearer $PUBLISH_TOKEN" \
  -F package=@my-app-1.0.0-1-x86_64.pkg.tar.zst
```

The public repository signing keys are at:

```text
https://repo.kingironman.dev/keys/linux-repo.asc
https://repo.kingironman.dev/keys/linux-repo.gpg
```

## GitHub Actions: publish a complete release

The reusable action accepts one JSON manifest, so a single Tauri release job
can upload every native artifact after its build finishes. Add this file to the
package repository as `linux-repo-manifest.json` (paths are relative to the
workflow workspace):

```json
[
  { "format": "debian", "package": "src-tauri/target/release/bundle/deb/my-app_1.0.0_amd64.deb" },
  { "format": "rpm", "package": "src-tauri/target/release/bundle/rpm/my-app-1.0.0-1.x86_64.rpm", "release": "41", "arch": "x86_64" },
  { "format": "arch", "package": "dist/my-app-1.0.0-1-x86_64.pkg.tar.zst", "arch": "x86_64" }
]
```

Then publish all entries with one step:

```yaml
- uses: KingIronMan2011/linux-repo@main
  with:
    token: ${{ secrets.LINUX_REPO_PUBLISH_TOKEN }}
    manifest: linux-repo-manifest.json
```

## Dokploy configuration

Create an Application from this repository, map `repo.kingironman.dev` to port
`8080`, and attach a named volume at `/data`. Set a long random `PUBLISH_TOKEN`
as an environment secret. The signing key is generated on the first start and
kept only in that persistent volume.

Use exactly one replica: package indexes are deliberately serialized to keep
metadata consistent.
