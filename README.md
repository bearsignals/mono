# mono

mono is a devtool that extends [conductor](https://conductor.build) and allows you to easily spawn parallel dev environments for each conductor workspace.

<details>
<summary>Motivation/Philosophy</summary>

coding agents have solved parallel codegen, but the bottlenecks of parallel software development (design, testing, verification, etc) remain largely unsolved. secondly, while coding agents run inference & work on tasks, there's not much to do besides wait. what if you could take advantage and take a stab at another feature?

even though a large majority of apps run **primarily** on web, and any given dev machine has at least 60,000 unused ports and a few cpu cores lying around, parallel evaluations is still hard. mono is an attempt to tackle part of this problem, and it does so by extending conductor and providing primitives that allow you to parallelize your dev process with low collision.

most tools fail completely because they ignore the constraints & subjectivity of software engineering, and try to be a magic wand.

mono **is not** a magic wand, but if you're aware of your **constraints**, it can help you build one in **<= 20mins**.

</details>

## What it doees?

- mono creates and manages a tmux session for each workspace(git worktree)
- mono injects specific environment variables into tmux session, which allow you to run stuff without collision.
- mono supports docker-compose, which allows each workspace to run isolated services (postgres, redis, telemetry-collectors)
- mono creates data directories for each workspace, thereby providing $HOME isolation.
- mono solves the heavy `node_modules/` & `target/` problem. No need for each workspace to recompile and redownload the internet for each workspace.
- mono provides a `~/.mono/mono.log` file which provides centralized observability for all your environments

## Install

```bash
curl -fsSL https://gwuah.github.io/mono/install.sh | sh
```

## Setup

In your project root, add this to your conductor `conductor.json`

```json
{
  "scripts": {
    "setup": "mono init",
    "run": "mono run",
    "archive": "mono destroy"
  }
}
```

## Configuration

In your project root, create a `mono.yml` and use these **optional** configurations to construct your dev environemt.

```yml
env:
  MONO_HOME: "${MONO_DATA_DIR}" # set the home directory for your service
  API_PORT: "$((5678 + MONO_ENV_ID))" # deterministically set the PORT for your backend service
  FRONTEND_PORT: "$((3000 + MONO_ENV_ID))" # deterministically set the PORT for your web service

compose_dir: backend # set the path to your docker componse file (only required if you're in a mono repo)

scripts:
  init: |
    cargo build
    cd web && npm install

  setup: |
    ln -sf "$MONO_ROOT_PATH/.env" "$MONO_ENV_PATH/.env"

  run: |
    cargo run --bin bibliotek -- -c config.yaml &
    cd web && npm run dev

  destroy: |
    run cleanup.sh
```

## How to integrate

The fastest way to leverage **mono** is to copy the readme, open claude-code (or any coding agent) in the root of your project, pipe this documentation to it, and ask it to preview all the changes that have to be made to your local dev setup, in order to get the best value out of mono. Show them your makefiles, dockerfiles, and any other important tooling you rely on. Work with the agent to port your devconfig.

## Architecture

```
                        CONDUCTOR
                 (worktrees + Claude Code)
                            │
        ┌───────────────────┼───────────────────┐
        ▼                   ▼                   ▼
   ┌─────────┐         ┌─────────┐         ┌─────────┐
   │ ws: foo │         │ ws: bar │         │ ws: baz │
   └────┬────┘         └────┬────┘         └────┬────┘
        │                   │                   │
        └───────────────────┼───────────────────┘
                            ▼
                          MONO
                            │
        ┌───────────────────┼───────────────────┐
        ▼                   ▼                   ▼
   ┌─────────┐         ┌─────────┐         ┌─────────┐
   │ mono-foo│         │ mono-bar│         │ mono-baz│
   │ :19100  │         │ :19200  │         │ :19300  │
   └─────────┘         └─────────┘         └─────────┘
   docker+tmux         docker+tmux         docker+tmux
        │                   │                   │
        └───────────────────┼───────────────────┘
                            ▼
                  ┌──────────────────┐
                  │  SHARED CACHE    │
                  │  node_modules/   │
                  │  target/         │
                  └──────────────────┘
```

Each workspace gets isolated ports, containers, and tmux session. Lastly, there is a shared cache they can pull from.

## Similar Tools

- [piko](https://github.com/bearsignals/piko)
- [okiro](https://github.com/ygwyg/okiro)

## Commentary

Currently, this tool is tightly coupled with conductor. However, there's no reason why it can't work with any copy of your project.I'm open to PRs that reduce this coupling and make it more generic. I like conductor.build because as someone who loves a good balance between GUI & TUI, they provide just enough primitives to hook into and enjoy both.
