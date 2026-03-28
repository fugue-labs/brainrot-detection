# brainrot-detection

A single Go binary that monitors YouTube on a PS5 (or any YouTube TV app), detects brainrot content using an LLM, plays a warning over Sonos, and turns off the TV if the content isn't changed within 10 seconds.

## How it works

```
PS5 YouTube App
      |
      | YouTube Lounge API (real-time event stream)
      v
  brainrot binary
      |
      |-- Fetches video title, channel, description from YouTube
      |-- Classifies content via GPT (free, uses ChatGPT subscription)
      |
      v
  BRAINROT? ──yes──> Play warning over Sonos (volume +30%)
                          |
                          | 10 second countdown
                          |
                     Still brainrot? ──yes──> Turn off TV via LG WebOS API
                          |
                          no ──> Crisis averted
```

**Key behaviors:**
- Pausing, seeking, or buffering does **not** cancel the warning
- Switching to another brainrot video does **not** cancel the warning
- The **only** way to cancel is to switch to content the LLM confirms is not brainrot
- If the TV is turned off and back on, the agent automatically reconnects

## What gets flagged

The LLM classifies content with a bias toward flagging. Things it catches:
- YouTube Shorts and TikTok compilations
- Roblox clickbait (Blox Fruits, Brookhaven, "BANNED" videos)
- Minecraft clickbait/drama (not genuine tutorials)
- MrBeast-style challenge/stunt content
- Skibidi toilet, sigma/rizz/gyatt meme content
- Satisfying/oddly satisfying compilations
- Reaction videos, Gacha Life, story time animations
- Content in any language matching these patterns

Things it allows:
- Quality kids' shows (Bluey, Numberblocks, Octonauts, Peppa Pig, Sesame Street, Daniel Tiger, etc.)
- Educational content (PBS Kids, Khan Academy Kids, SciShow Kids, Crash Course Kids)
- Calm Minecraft building/survival content
- Drawing, craft, LEGO, and cooking tutorials
- Nature documentaries and animal videos
- Children's music, read-alouds, audiobooks
- Family movies and Pixar/Disney/Ghibli content
- Real music, sports, and genuine gaming tutorials

## Requirements

- **Go 1.25+** to build
- **Sonos soundbar** connected to your TV (tested with Arc Ultra)
- **LG WebOS TV** (for power-off control)
- **PS5** (or any device running YouTube with "Link with TV code" support)
- **ChatGPT Plus/Pro/Team subscription** (for free LLM classification via OAuth)

## Setup

### 1. Build

```bash
git clone https://github.com/trevorprater/brainrot-detection
cd brainrot-detection
go build -o brainrot .
```

### 2. Configure

Copy the sample config:

```bash
cp config.sample.json config.json
```

Edit `config.json` with your device IPs:

```json
{
  "sonos_ip": "192.168.0.XXX",
  "lg_tv_ip": "192.168.0.XXX",
  "lg_tv_key": "",
  "warning_delay_seconds": 10
}
```

**Finding your device IPs:**
- **Sonos**: Open the Sonos app → Settings → System → your speaker → IP address
- **LG TV**: Settings → Network → Wi-Fi → your network → IP address

The `lg_tv_key` is populated automatically on first run — you'll see a pairing prompt on your TV. Accept it once, and the key is saved.

### 3. Run

```bash
./brainrot
```

**First run does two things:**

1. **ChatGPT OAuth** — opens your browser to log into your ChatGPT account. This is a one-time login that gives the agent free access to GPT for classification. Credentials are saved to `~/.golem/auth.json`.

2. **PS5 YouTube pairing** — on your PS5, open YouTube → Settings → "Link with TV code". Enter the 12-digit code when prompted. Auth tokens are saved to `config.json` for future runs.

After setup, the agent runs silently and reconnects automatically if the TV is power-cycled.

### 4. Customize the warning audio

The warning audio is embedded in the binary at build time. To change it:

1. Replace `warning.mp3` with your own audio file
2. Rebuild with `go build -o brainrot .`

The default warning uses an ElevenLabs-generated British broadcaster voice:

> *"Attention. Brainrot content has been detected. You have ten seconds to change what you are watching, or the TV will be turned off."*

## Built with gollem

The brainrot classification is powered by [gollem](https://github.com/fugue-labs/gollem), a Go agent framework with compile-time type safety and structured outputs. It made the LLM integration trivial — define a struct, get back typed results:

```go
type BrainrotClassification struct {
    IsBrainrot bool   `json:"is_brainrot"`
    Confidence string `json:"confidence" jsonschema:"enum=high|medium|low"`
    Reason     string `json:"reason"`
}

agent := core.NewAgent[BrainrotClassification](provider,
    core.WithSystemPrompt[BrainrotClassification]("You are a parental content filter..."),
)

result, _ := agent.Run(ctx, "Title: Skibidi Toilet Episode 73...")
if result.Output.IsBrainrot {
    // play warning, turn off TV
}
```

Gollem also handles the ChatGPT subscription OAuth (no API key or billing needed), token refresh, and structured output schema generation from Go structs. Zero runtime dependencies in core.

## Architecture

Single binary, no runtime dependencies. Everything is embedded or handled via HTTP:

| Component | Implementation |
|---|---|
| YouTube monitoring | [goyoutubelounge](https://github.com/d6o/GoYoutubeLounge) — real-time Lounge API events |
| Video metadata | YouTube page scraping (no API key needed) |
| Content classification | [gollem](https://github.com/fugue-labs/gollem) agent with GPT structured output |
| LLM auth | ChatGPT subscription via OAuth PKCE (no API key or billing) |
| Sonos control | Direct UPnP/SOAP over HTTP |
| TV power control | [webostv](https://github.com/snabb/webostv) — LG WebOS websocket API |
| Warning audio | Embedded MP3 via `go:embed`, served to Sonos over local HTTP |

## Adapting for other setups

**Different TV brand?** The LG WebOS `turnOffTV()` function in `main.go` is ~15 lines. Replace it with your TV's API:
- Samsung: [samsungctl](https://github.com/jaruba/ha-samsungtv-tizen)
- Sony Bravia: HTTP API with pre-shared key
- Roku TV: `POST http://<ip>:8060/keypress/PowerOff`

**No Sonos?** Replace `playWarningOnSonos()` with any audio playback method. The warning MP3 is served on `localhost:8769/warning.mp3`.

**Not a PS5?** The YouTube Lounge API works with any device that supports "Link with TV code": smart TVs, Chromecast, Xbox, Fire TV, Roku.

## License

MIT
