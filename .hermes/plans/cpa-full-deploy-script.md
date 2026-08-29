# CPA Full Deployment Script Plan

## Goal

Create one local Python deployment entrypoint under `deploy/` for deploying the CPA main binary and every repository-managed plugin to Oracle 01, with complete local logs and automatic rollback.

## Scope

- Build and deploy the CPA main binary.
- Discover and build all six plugin modules under `plugins/src/*/go.mod` with `-buildmode=c-shared`.
- Preserve the two external online plugins, `codexcomp` and `gemini-cli`, and verify that they still register after restart.
- Preserve `config.yaml`, `plugin-data`, and every auth JSON.
- Track `deploy/deploy_cpa_full.py`; ignore only generated deployment logs and `__pycache__`.
- Accept no CLI parameters. Use the fixed Oracle 01 production endpoint defaults.
- Do not execute a production deployment while creating the script.

## Workflow

1. Validate local repository, SSH key, required commands, Git revision, dirty state, and plugin source inventory.
2. Fetch upstream and discover the newest stable release tag; reserve the next four-digit build number under a remote lock.
3. Package the current tracked working tree with LF-normalized text files; refuse untracked source files outside explicitly ignored operational directories.
4. Upload the source archive, remote helper, and generated remote deployment script.
5. On the server, record service/PID/version, disk usage, config hash, auth count/validity, and online plugin hashes.
6. Build the ARM64 CGO main binary and all six repository plugins.
7. Verify binary architecture, build metadata, build mode, and SHA-256 values.
8. Stop `cli-proxy-api.service` and create a full rollback archive containing the main binary, config, all plugins, plugin data, and auth directory.
9. Validate backup gzip/tar integrity, required members, auth count, and every auth JSON before changing live files.
10. Install the main binary and six plugins through same-directory temporary files plus atomic rename.
11. Start the service and poll for up to 90 seconds.
12. Verify PID/state, `/`, `/healthz`, management headers, installed hashes, config preservation, auth count/JSON validity, all eight plugin registrations, journal errors, and required xAI Responses markers.
13. Roll back from the verified archive on any post-stop failure.
14. Save the local master log, remote deployment log, and journal log beside the script; remove transient source/build artifacts.

## Safety Decisions

- Accept no CLI parameters; derive the version from the newest stable upstream tag and a remotely locked four-digit build sequence.
- Reserve the build number before upload/build; a failed attempt consumes its number so concurrent or retried runs cannot reuse it.
- A latest stable tag that is not merged into `HEAD` is logged as a warning, not a deployment blocker; the artifact source remains the current working tree.
- Use the current working-tree content for tracked files. If dirty, label the build revision with a source hash suffix instead of claiming an exact clean commit.
- Refuse untracked source files so they cannot be silently omitted.
- Never copy local `config.yaml`, `.env`, auth files, or ignored runtime data into the source archive.
- Do not delete backups automatically.
- Do not commit, push, or deploy automatically.

## Verification

- Rely on file-write syntax validation for the Python script.
- Check generated files with `git check-ignore`; verify `deploy/deploy_cpa_full.py` is not ignored.
- Run semantic Git path checks with `core.autocrlf=false` so line-ending warnings cannot become false conflict paths.
- Inspect `git diff --check` and final `git status`.
- Do not run unit/integration tests or execute the deployment script without explicit user instruction.

## Revision Summary

### rev.1

- Removed the mandatory `--version` argument and `--yes` confirmation flag.
- Added automatic stable-tag discovery and remotely locked build-number allocation.
- Changed `.gitignore` to track the deployment script while ignoring generated logs and `__pycache__`.

### rev.2

- Changed an unmerged latest stable tag from a hard failure to an explicit warning.
- Kept upstream fetch, divergence counts, and merge-state logging before version reservation.

### rev.3

- Prevented `git diff` line-ending warnings from being misread as unmerged paths.
