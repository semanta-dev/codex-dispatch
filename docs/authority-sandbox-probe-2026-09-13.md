# Codex authority exclusion probe — 2026-09-13

The CTO review used installed `codex-cli 0.154.0` on Linux with local scratch
sentinels. No model calls, real credentials, or global configuration changes
were involved. Scratch directories were removed after the probes.

`codex sandbox -P :workspace -C "$scratch_workspace" -- /usr/bin/python3 probe.py`
is the working local syntax on this version. The older `sandbox linux` spelling
attempts to execute a program named `linux`; `-c sandbox_mode=...` alone does not
select a permission profile for this command.

| Profile | TMPDIR | Authority read | Authority write | Workspace write |
|---|---|---|---|---|
| Built-in `:workspace` | Unset | Allowed | Denied | Allowed |
| Built-in `:workspace` | Authority scratch | Allowed | Allowed | Allowed |
| Extends `:workspace`, authority-root deny | Unset | Denied | Denied | Allowed |
| Same authority deny | Authority scratch | Allowed | Allowed | Allowed |
| Authority deny and `:tmpdir` deny | Unset | Denied | Denied | Allowed |
| Both denies | Authority scratch | Child startup rejected | Child startup rejected | Not executed |

The explicit test profile used an inline TOML table:

```toml
permissions={authority_probe={extends=":workspace",filesystem={":tmpdir"="deny","/absolute/authority/root"="deny"},network={enabled=false}}}
```

A more specific TMPDIR grant can override a parent-path deny, consistent with
the [Codex permission model](https://developers.openai.com/codex/permissions/).
Integration must sanitize temporary directories into controller-owned scratch
and reject writable roots overlapping authority. A parent deny by itself is
insufficient. Unrestricted reviewed execution must fail closed.

The default account-store creation path now rejects the observed inherited
temporary-root overlap before publishing recovery data. Regressions cover
equal, ancestor, descendant and symlink-alias temporary roots. Explicit custom
store roots used by isolated authority tests do not invoke this account-store
check. This is an incremental guard; the app-server sandbox integration is
still unqualified.

These results establish a concrete confidentiality/integrity gap in relying on
the default sandbox. They do not qualify dispatch's app-server integration,
resume, detached/background ownership, another account, or another operating
system. The production authority boundary remains OPEN and promotion NO-GO.
