# Dotfiles

cld personalizes every managed container from your host's `~/.dotfiles`, the same
way VS Code Dev Containers does. It is enabled by default and is a no-op if you
have no `~/.dotfiles`.

## How it works

When the daemon provisions a container, `install_dotfiles` runs as a best-effort
step (a failure is logged, never blocks the session):

1. It reads your host `~/.dotfiles` through the **read-only mount** `cld install`
   adds (your home — only your home, not the whole host root — is mounted at
   `/host-home` inside the daemon container; see `docs/architecture.md`). No
   mount → nothing to read → skipped.
2. It copies the tree into the container at `~/.dotfiles`, owned by the container
   user.
3. Then, depending on the tree:
   - **`install.sh` present** → cld normalizes its line endings, makes it
     executable, and runs it **as the container user**, with `~/.dotfiles` as the
     working directory and `$HOME` set, honoring its shebang.
   - **no `install.sh`** → cld symlinks the tree's top-level dotfiles into `$HOME`
     with `ln -sfn` (skipping `.`, `..`, and `.git`).

It runs **once per container** (re-runs when a container is recreated). Because it
copies with `docker cp`, **symlinks inside `~/.dotfiles` are not followed** — put
real files in `~/.dotfiles` (or do the linking yourself in `install.sh`).

### Symlink fallback rules

When there is no `install.sh`, for each top-level entry `~/.dotfiles/.X`:

| Existing `$HOME/.X`         | Result                                             |
| --------------------------- | -------------------------------------------------- |
| nothing                     | `~/.X` → `~/.dotfiles/.X`                         |
| a symlink                   | replaced → `~/.dotfiles/.X`                       |
| a **real file**             | replaced → `~/.dotfiles/.X`                       |
| a **real directory**        | **left untouched** (not merged, not clobbered)     |

A real directory is skipped on purpose: linking into it would create a stray
`~/.X/.X`, and replacing it could drop container-provisioned content (for example
`~/.local/bin`, which cld itself populates). If you need directory-level control,
use `install.sh`.

## Enabling / disabling

Enabled by default. To turn it off, in `cld.yaml`:

```yaml
dotfiles:
  disabled: true
```

## Example — `install.sh` (recommended)

A dotfiles repo with an install script gives you full control (this is the most
portable layout and also what works under VS Code Dev Containers).

```
~/.dotfiles/
├── install.sh
├── gitconfig
├── bashrc
└── config/
    └── nvim/
        └── init.lua
```

```sh
#!/bin/sh
# ~/.dotfiles/install.sh — run inside the container, CWD is ~/.dotfiles, $HOME set.
set -e
ln -sfn "$PWD/bashrc"  "$HOME/.bashrc"
ln -sfn "$PWD/gitconfig" "$HOME/.gitconfig"
mkdir -p "$HOME/.config"
ln -sfn "$PWD/config/nvim" "$HOME/.config/nvim"
```

Notes:
- The executable bit is preserved for copied files, so helper scripts
  `install.sh` calls (`./scripts/setup`, `./bin/*`) stay runnable.
- `install.sh` may use any interpreter via its shebang (`#!/bin/bash`,
  `#!/usr/bin/env python3`, …) as long as that interpreter exists in the
  container image.

## Example — no `install.sh` (symlink fallback)

If you keep dotfiles at the top level and no `install.sh`, cld links them for you:

```
~/.dotfiles/
├── .bashrc
├── .vimrc
└── .gitconfig
```

Result in the container: `~/.bashrc`, `~/.vimrc`, `~/.gitconfig` become symlinks
into `~/.dotfiles`. A top-level directory such as `.config/` is only linked if
`$HOME` does not already contain a real `.config/` (see the table above).

## `.gitconfig` and cld's built-in git sharing

cld **already** shares your host git identity into every container, independently
of dotfiles (VS Code parity). It is important to understand how the two interact,
because putting `.gitconfig` in your dotfiles does *not* simply "win".

**The built-in path.** On `cld it` / `cld up`, cld stages your host `~/.gitconfig`
into its cache, then during provisioning writes a **sanitized** copy to
`~/.cld/claude/gitconfig` inside the container and points the **claude session**
at it with `GIT_CONFIG_GLOBAL`. That env var is set **only in the claude session's
environment** (claude and the subprocesses it spawns, e.g. its Bash tool) — it is
never written to a shell profile or exported container-wide.

**The dotfiles path.** A `.gitconfig` from your dotfiles lands at `~/.gitconfig`
(git's *default* global path).

Git uses `GIT_CONFIG_GLOBAL` when it is set, and otherwise falls back to
`~/.gitconfig`. So who wins depends on **who runs git**:

| Who runs git                                   | You have a host `~/.gitconfig` | You have no host `~/.gitconfig` |
| ---------------------------------------------- | ------------------------------ | ------------------------------- |
| **claude** (and its Bash tool)                 | cld's sanitized copy (via `GIT_CONFIG_GLOBAL`) — the dotfiles `~/.gitconfig` is **ignored** | dotfiles `~/.gitconfig` |
| **your own shell** (VS Code terminal, `docker exec`, `install.sh`) | dotfiles `~/.gitconfig` | dotfiles `~/.gitconfig` |

In short:

- For **claude's own git**, the host-staged config is authoritative whenever you
  have a host `~/.gitconfig`; a dotfiles `~/.gitconfig` only affects claude when
  you have **no** host gitconfig.
- For **your interactive shells**, the dotfiles `~/.gitconfig` always wins
  (nothing sets `GIT_CONFIG_GLOBAL` there).

They do not overwrite each other on disk — cld's copy is at
`~/.cld/claude/gitconfig`, the dotfiles copy is at `~/.gitconfig` — so the only
question is which file git reads, per the table above.

### What cld changes when it copies your gitconfig

The built-in path is **not** a verbatim copy: cld runs `strip_credential_helpers`,
which removes every `helper = …` entry under `[credential]`. Host credential
helpers (`osxkeychain`, `manager`, `gopass`, `wincred`, …) are host-only binaries
that do not exist in the container; leaving them in would make git fail HTTPS auth
with `git: 'credential-osxkeychain' is not a git command`. In-container git uses
the **forwarded ssh-agent** for SSH remotes instead. Everything else — identity
(`user.name`/`user.email`), signing config, aliases, `core.*` — carries over
unchanged.

> **Caveat:** the **dotfiles** path does **no** sanitization. A `.gitconfig` you
> put in `~/.dotfiles` is copied verbatim, credential helpers and all — so in a
> plain container shell it can hit the "not a git command" error above. Prefer
> cld's built-in sharing for your host gitconfig, and keep `[credential]` helpers
> out of any `.gitconfig` you ship via dotfiles.

## Caveats

- **No sanitization.** Unlike cld's user-default config (which drops secret/host-
  only keys) and its gitconfig path (which strips credential helpers), dotfiles
  are copied **verbatim**. Keep secrets (tokens, private keys) out of
  `~/.dotfiles`.
- **Symlinks in `~/.dotfiles` are not followed** by the copy — stow/bare-repo
  layouts that are symlink farms won't materialize. Use real files, or run `stow`
  from `install.sh`.
- **Windows line endings** in `install.sh` are handled (CR is stripped before it
  runs).
- **Real directories in `$HOME`** with the same name as a top-level dotfile are
  left untouched by the symlink fallback (see the table above).
- Requires the daemon to run via `cld install` / docker-compose (that is what adds
  the read-only host-home mount); it never reads the host home when the daemon is
  run some other way.
