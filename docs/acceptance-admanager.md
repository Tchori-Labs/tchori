# Acceptance checklist: terraform-provider-admanager under tchori

Evidence artifact for engine-spec success criterion #2
(`docs/superpowers/specs/2026-07-09-tchori-engine-design.md` in the company
repo): `terraform-provider-admanager` — plugin protocol 6, built on
terraform-plugin-framework — runs under tchori against a Google Ad Manager
**test network**. Manual until automatable. Complete every box, fill in the
run record, and commit the updated file as the acceptance evidence.

## Prerequisites

- [ ] A Google Ad Manager **test network** (never a production network) and
      its network code.
- [ ] Credentials authorized for that network, provided via environment
      variables only — tchori configs reference secrets as
      `{"env": "VAR_NAME"}`; never paste credentials into config files.
- [ ] `terraform-provider-admanager` built locally from source into a plugin
      dir: `go build -o <plugin-dir>/terraform-provider-admanager .`
      (record the provider repo commit SHA below).
- [ ] A `tchori` binary on PATH (`tchori version` — record below).

## Checklist

Work in a fresh, empty directory. `<plugin-dir>` is the directory holding
the locally built provider binary; every provider-touching command below
takes `--plugin-dir <plugin-dir>`.

- [ ] **1. Discovery from -plugin-dir.** Write `main.tchori.json` declaring
      the admanager provider (discovery is by binary name from
      `<plugin-dir>`, so no registry install is needed) and exactly one test
      resource of the provider's simplest resource type, configured for the
      test network.
- [ ] **2. Validate.** `tchori validate --plugin-dir <plugin-dir>` exits 0:
      the provider launches (protocol-6 handshake succeeds), returns
      schemas, and accepts the resource config.
- [ ] **3. Plan.** `tchori plan --plugin-dir <plugin-dir> -out plan.json`
      exits 2; `plan.json` has `"format_version": "1.0"` and summary
      `{"create": 1, "update": 0, "delete": 0, "replace": 0}`.
- [ ] **4. Apply.** `tchori apply --plugin-dir <plugin-dir> plan.json`
      exits 0; the resource is visible in the GAM test network (UI or API);
      `tchori state show <address>` prints its attributes including
      server-computed ones.
- [ ] **5. Idempotence.** `tchori plan --plugin-dir <plugin-dir>` exits 0
      and prints "No changes".
- [ ] **6. Destroy.** `tchori destroy --plugin-dir <plugin-dir> -out destroy.json`
      exits 2; `tchori apply --plugin-dir <plugin-dir> destroy.json` exits 0;
      the resource is gone from the test network and `tchori state list`
      prints nothing.
- [ ] **7. Diagnostics.** Force one provider-side error (e.g. an invalid
      field value) and confirm it surfaces as a structured JSON diagnostic
      on stderr with exit 1.

## Run record

| Field | Value |
| --- | --- |
| Date | |
| tchori version | |
| Provider commit SHA | |
| GAM network code (test) | |
| Resource type exercised | |
| Result (pass/fail) | |
| Notes / deviations | |
