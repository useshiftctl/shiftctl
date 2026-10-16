# shiftctl

A local-first CLI for migrating code repositories between Git platforms — GitHub, GitLab, Bitbucket, Azure DevOps.

shiftctl uses AI to analyze your repos, assign risk levels, and generate a staged migration plan. You review and approve every step before anything runs. A browser companion at `localhost:4242` guides you through the full process without needing to memorize commands.

Everything runs on your machine. No data leaves without your explicit action.

---

## What it does

1. **Assess** — scans all repos on your source platform and uses Claude (Anthropic AI) to assign a risk level and confidence score to each one
2. **Plan** — groups repos into stages (low risk first, high risk last) with notes you can adjust
3. **Approve** — you sign off on each stage before it runs
4. **Execute** — mirrors repos to the destination platform, stage by stage
5. **Verify** — confirms every repo in the stage is reachable on the destination
6. **Decommission** — removes the source once everything is verified

---

## Prerequisites

- **macOS** (tested; Linux and Windows may work but are not officially supported yet)
- **Go 1.23+** (only needed if installing via `go install`)
- **An Anthropic API key** — needed for the `assess` step only. Get one at [console.anthropic.com](https://console.anthropic.com). The Anthropic API has no ongoing free tier — new accounts receive $5 in credits on signup, which is enough to run assessments on a typical repo set. Set it as an environment variable:
  ```sh
  export ANTHROPIC_API_KEY=sk-ant-...
  ```
- **A source platform token** — a personal access token from GitHub or GitLab with read access to your repos. shiftctl will prompt you for this and validate it during setup.

---

## Install

**Option A — go install**
```sh
go install github.com/useshiftctl/shiftctl@latest
```

**Option B — download a binary**

Download the latest release for your platform from the [Releases page](https://github.com/useshiftctl/shiftctl/releases), then move the binary onto your `$PATH`:
```sh
mv shiftctl /usr/local/bin/shiftctl
```

---

## Quick start

```sh
# 1. Set your Anthropic API key
export ANTHROPIC_API_KEY=sk-ant-...

# 2. Connect to your source platform — shiftctl walks you through it
shiftctl connect

# 3. Open the browser companion (use the migration name you entered in step 2)
shiftctl serve <migration-name>
```

Then open [http://localhost:4242](http://localhost:4242) — the companion guides you through assessment, planning, approval, execution, verification, and decommission.

---

## Commands

| Command | What it does |
|---|---|
| `shiftctl connect` | Set up a new migration (interactive) |
| `shiftctl serve <name>` | Open the browser companion for a migration |
| `shiftctl assess <name>` | Run AI assessment on source repos |
| `shiftctl plan <name>` | Generate a staged migration plan |
| `shiftctl approve <name>` | Approve the next execution stage |
| `shiftctl execute <name>` | Run the approved stage |
| `shiftctl verify <name>` | Verify repos are reachable on the destination |
| `shiftctl decommission <name>` | Remove the source and mark the migration complete |
| `shiftctl list` | List all migrations on this machine |
| `shiftctl status <name>` | Show current status of a migration |
| `shiftctl disconnect --purge` | Remove all local data for a migration |

---

## Supported platforms

| Platform | Source | Destination |
|---|---|---|
| GitHub | ✓ | ✓ |
| GitLab | ✓ | ✓ |
| Bitbucket | — | ✓ |
| Azure DevOps | — | ✓ |

---

## Early access

shiftctl is in early access. If something breaks or behaves unexpectedly, please [open an issue](https://github.com/useshiftctl/shiftctl/issues). Feedback on the migration flow, the companion UI, and edge cases you hit is especially welcome.
