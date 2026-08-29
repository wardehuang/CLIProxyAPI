# CPA Full Deployment Script Plan

## Goal

Create one local Python deployment entrypoint under `deploy/` for deploying the CPA main binary and every repository-managed plugin to Oracle 01, with complete local logs and automatic rollback.

## Scope

- Build and deploy the CPA main binary.
- Discover and build all six plugin modules under `plugins/src/*/go.mod` with `-buildmode=c-shared`.
- Preserve the two external online plugins, `codexcomp` and `gemini-cli`, and verify that they still register after restart.
- Preserve `config.yaml`, `plugin-data`, and every auth JSON.
- Add `/deploy/` to `.gitignore`.
- Do not execute a production deployment while creating the script.

## Workflow

1. Validate local repository, SSH key, required commands, version argument, Git revision, dirty state, and plugin source inventory.
2. Package the current tracked working tree with LF-normalized text files; refuse untracked source files outside explicitly ignored operational directories.
3. Upload the source archive, remote helper, and generated remote deployment script.
4. On the server, record service/PID/version, disk usage, config hash, auth count/validity, and online plugin hashes.
5. Build the ARM64 CGO main binary and all six repository plugins.
6. Verify binary architecture, build metadata, build mode, and SHA-256 values.
7. Stop `cli-proxy-api.service` and create a full rollback archive containing the main binary, config, all plugins, plugin data, and auth directory.
8. Validate backup gzip/tar integrity, required members, auth count, and every auth JSON before changing live files.
9. Install the main binary and six plugins through same-directory temporary files plus atomic rename.
10. Start the service and poll for up to 90 seconds.
11. Verify PID/state, `/`, `/healthz`, management headers, installed hashes, config preservation, auth count/JSON validity, all eight plugin registrations, journal errors, and required xAI Responses markers.
12. Roll back from the verified archive on any post-stop failure.
13. Save the local master log, remote deployment log, and journal log beside the script; remove transient source/build artifacts.

## Safety Decisions

- Require `--version` and `--yes`; never invent a version.
- Use the current working-tree content for tracked files. If dirty, label the build revision with a source hash suffix instead of claiming an exact clean commit.
- Refuse untracked source files so they cannot be silently omitted.
- Never copy local `config.yaml`, `.env`, auth files, or ignored runtime data into the source archive.
- Do not delete backups automatically.
- Do not commit, push, or deploy automatically.

## Verification

- Rely on file-write syntax validation for the Python script.
- Check `/deploy/` with `git check-ignore`.
- Inspect `git diff --check` and final `git status`.
- Do not run unit/integration tests or execute the deployment script without explicit user instruction.
