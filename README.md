# Baton

Local AI engines, ComfyUI, games and game streaming take turns with your
NVIDIA GPUs on Windows, instead of crashing each other with out-of-memory
errors.

One Go binary (`baton.exe`): no installer, no cloud, no C toolchain. It runs
in the background with a tray icon and a web dashboard.

## What it does

- **Engines take turns.** When something needs the GPUs, baton waits for the
  current holder to finish its work (a generation, a ComfyUI job), releases it,
  checks that the driver shows the memory free, then starts the next engine.
- **Loads outside baton count too.** Pick a model in Unsloth Studio's own UI,
  or run TabbyAPI's `start.bat` yourself, and baton releases whatever else
  holds the cards: the newest load wins.
- **Games free the GPUs.** A game from Steam, Epic, GOG, Xbox, EA, Ubisoft, an
  emulator, or a folder you add stops every engine (running work gets 15 s),
  and the LLM comes back after you quit.
- **Streams stay smooth.** While Apollo/Sunshine or Steam Remote Play is
  streaming (an NVENC session), ComfyUI is freed and new jobs wait; the LLM
  stays loaded while the encoding card keeps enough VRAM free.
- **Sees every process on every card.** NVIDIA's driver reports no
  per-process memory on Windows (WDDM), so baton reads Windows' own GPU
  counters (what Task Manager uses) and names every process holding VRAM.
- **System RAM too**: in use, cache, commit charge, the biggest users grouped
  by app, and each engine's own RAM.
- **One API for your LLMs.** An OpenAI-compatible gateway: ask for a model and
  baton brings up the engine that serves it, sending keep-alives while it loads
  so clients don't time out.
- **Engine logs and stats.** TabbyAPI's output in the dashboard, with the last
  request's decode and prompt speed, draft (MTP) acceptance and time to first
  token.
- **From your phone** over Tailscale, if you want it.

## Requirements

- Windows 10 or 11, NVIDIA GPU(s) with a current driver (`nvidia-smi` works).
- [Go 1.26+](https://go.dev/dl/) to build it.
- Any of the supported engines: [TabbyAPI](https://github.com/theroyallab/tabbyAPI),
  [Unsloth Studio](https://unsloth.ai), [ComfyUI](https://github.com/comfyanonymous/ComfyUI)
  (portable or Desktop).

## Set up

```powershell
git clone <this repo> baton
cd baton
go build -o baton.exe ./cmd/baton
copy baton.example.toml baton.toml    # then edit it: your engines, paths and GPUs
.\baton.exe config                     # checks the config
.\baton.exe                            # starts baton and opens the dashboard
.\baton.exe autostart on               # optional: start at login, no window
```

`baton.toml` lives next to `baton.exe`. Each `[[engine]]` says where the
engine answers, how to start it and which GPUs it uses (NVIDIA's numbering,
as in `nvidia-smi`). Delete the engines you don't use.

To run `baton` from any terminal, add its folder to your user `PATH`.

## Modes

| Mode | When | What happens |
|---|---|---|
| normal | nothing below | engines take turns on demand |
| stream | Apollo/Sunshine or Steam has an NVENC session, or `baton hook stream-start` | ComfyUI finishes its job (up to 60 s), is freed, and new jobs wait; the LLM stays while the encoding card has ≥ `min_free_mib` free |
| game | a game process runs, or `baton hook game-start` | every engine is released; the gateway answers 503 |
| hold | `baton hold`, the tray's *Hold GPUs*, or the dashboard | same as game, until you release it |

A game or stream must be gone for 20 s (`linger`) before baton restores the
LLM, so launchers restarting a game don't bounce it. ComfyUI jobs that waited
run first; the LLM comes back after they finish.

**Strict release.** After releasing an engine, baton waits up to
`release_wait` (60 s) for the driver to show its memory free. If it never does
(a stuck unload, a process that won't die), the engine is marked **aground**:
baton starts nothing else on those GPUs, since that could run out of memory,
and says why (the gateway answers `503` with `code: engine_aground`; ComfyUI
jobs wait). The mark clears by itself once the memory frees. **Force stop** on
the engine's card, or `baton reclaim ENGINE`, ends its server whatever its
release mode. Games and holds still release the other engines.
`strict_release = false` brings back the old behaviour: warn and start the
next engine anyway.

## Engines

**TabbyAPI.** Baton starts `start.bat` hidden and stops it by ending its
process tree, so a restart reloads exactly what `config.yml` says. If you start
TabbyAPI yourself, baton still finds it by its folder and port. Its output goes
to `%LOCALAPPDATA%\baton\logs\tabby.log`:

- **Log** on the tabby card in the dashboard shows it live, with the last
  request's stats on top (the card shows them too).
- `baton logs tabby -f` follows it in a terminal.
- `show_window = true` gives TabbyAPI its own console window instead.

When baton started TabbyAPI, a switch waits for exactly the requests it is
generating, read from this log, including ones sent to its port directly. If
you started it yourself (or with `show_window`) there is no log to read, and
baton goes by its GPU load instead.

**Unsloth Studio.** Studio stays open; baton unloads and reloads its model over
Studio's API, which needs a key: create one in Studio (Settings → API keys),
then `setx UNSLOTH_API_KEY "<key>"`. Without a key baton can still free the
GPUs (it stops the llama-server Studio runs the model in), but it can't load
the model back. Models the gateway may load by name go in `baton.toml` as
`[[engine.model]]`.

**ComfyUI.** Release waits for the queue, then calls ComfyUI's `/free`. Baton
follows ComfyUI Desktop when it moves to another port. Install the **gate
node** so jobs wait for the GPUs instead of grabbing them, however they're
queued (the UI, the API, an agent): copy `integrations/comfyui-baton` into
ComfyUI's `custom_nodes` folder (for ComfyUI Desktop, the `custom_nodes`
folder under the base path you chose). When baton isn't running, jobs run as
they always did; Cancel in ComfyUI skips a waiting job.

## Games

Click **Games** in the dashboard (or *Manage games…* in the tray):

- **Library folders**: every program inside counts as a game (`D:\Games`).
- **Single games**: one program, wherever it is installed.
- **Never count**: launchers and tools that sit next to games.
- **Using your GPU**: programs holding VRAM that baton doesn't recognize, with
  one-click *Add game* / *Ignore*.
- **Built-in launchers**: Steam, Epic, GOG, Xbox, EA, Ubisoft and emulators,
  on by default.

*Add folder…* and *Add game…* open Windows' own picker (it may open behind the
browser), or paste a path. Changes apply at once and are saved in `games.json`
next to `baton.toml`. Regex rules for anything unusual go in `baton.toml` as
`[[claim]]`.

## Gateway

Point OpenAI-compatible clients (OpenWebUI, Hermes, scripts) at:

```
base URL  http://127.0.0.1:9799/v1
API key   the output of: baton key
models    each engine's model plus the aliases in `serves` and [[engine.model]]
```

A model that isn't loaded is started for the request. Streaming requests get
`: baton: starting tabby, 10s` comments while they wait. During a game or hold
the gateway answers `503` with `code: gpu_reserved` and `Retry-After: 60`; while
an engine is aground, `503` with `code: engine_aground` and `Retry-After: 30`.

## Streaming (Apollo, Sunshine, Moonlight, Steam Remote Play)

Nothing to set up: baton sees the stream's NVENC session. To switch modes
before the encoder starts, you can also add prep commands in Apollo/Sunshine
(Configuration → General → Command Preparations):

```
do:   "<path to>\baton.exe" hook stream-start --source apollo
undo: "<path to>\baton.exe" hook stream-end --source apollo
```

`baton hook` always exits 0, so it never stops a stream from starting.

## From your phone (Tailscale)

Set `[remote] tailscale = true` and baton also answers on this PC's Tailscale
address, for Tailscale devices only: your LAN still can't reach it. The
dashboard shows the addresses to use under *On your phone*. Run
`baton firewall` once (it asks for admin) so Windows Firewall lets Tailscale
devices in. The dashboard has no password, so anyone on your tailnet can use
it; the gateway still needs its key.

## Commands

```
baton                                         start baton and open the dashboard
baton start | stop | daemon [--no-tray]       run it
baton status [--json] | gpus | events [-f]    look
baton logs ENGINE [-f] [-n N]                 an engine's output
baton use ENGINE [--model M]                  give an engine the GPUs
baton release ENGINE | free                   give them back
baton reclaim ENGINE                          force-stop an engine stuck holding VRAM
baton hold [--reason R] [--for 2h] | unhold   reserve the GPUs yourself
baton hook stream-start|stream-end|game-start|game-end [--source S]
baton key | config | autostart on|off | firewall | version
```

## Files

| Path | What |
|---|---|
| `baton.toml` (next to the exe) | your config (start from `baton.example.toml`) |
| `games.json` (next to the exe) | the game list you edit in the dashboard |
| `%LOCALAPPDATA%\baton\events.jsonl` | everything baton did and why |
| `%LOCALAPPDATA%\baton\logs\<engine>.log` | output of engines baton started |
| `%LOCALAPPDATA%\baton\gateway.key` | gateway API key |
| `%LOCALAPPDATA%\baton\state.json` | pending restore and holds, kept across restarts |

## Two cards, fewer swaps

Engines that share a card take turns; engines on different cards don't. With
two GPUs you can often avoid swapping entirely: for example ComfyUI on GPU 0
(`gpus = [0]`, `--cuda-device 0`) and a small Unsloth model on GPU 1
(`gpus = [1]`, `gpu_ids = [1]` in its `[engine.model]` extra). Only a model
that needs both cards then triggers a swap.

## Status

Young and built for one workstation first: tested daily on Windows 11 with an
RTX 5070 Ti and an RTX 3060 (driver 617), running TabbyAPI (ExLlamaV3),
Unsloth Studio, ComfyUI Desktop, Steam/Epic/standalone games and Apollo
streaming. Linux is not supported. Issues and pull requests are welcome.

## Uninstall

`baton autostart off`, `baton stop`, delete the gate node from ComfyUI's
`custom_nodes`, this folder, and `%LOCALAPPDATA%\baton`. If you ran
`baton firewall`, remove the "Baton (Tailscale devices)" rule in Windows
Defender Firewall.

## Acknowledgements

Baton was inspired by [Berth](https://github.com/T-Crypt/berth) by T-Crypt, a
VRAM lease broker for Linux, and the
[r/LocalLLaMA discussion](https://www.reddit.com/r/LocalLLaMA/comments/1xahi82/) around it.
Ideas taken from Berth: engines leasing the card in priority order, waiting
for a ComfyUI job to finish before a handoff, treating games as holders of the
card, and the launcher-folder rules for spotting games. Baton is a separate
implementation (no Berth code), written for Windows and multiple GPUs.

## License

Apache License 2.0. See [LICENSE](LICENSE).
