package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lounge "github.com/d6o/goyoutubelounge"
	"github.com/d6o/goyoutubelounge/auth"
	"github.com/d6o/goyoutubelounge/event"
	"github.com/fugue-labs/gollem/core"
	openaiauth "github.com/fugue-labs/gollem/auth/openai"
	openaiprovider "github.com/fugue-labs/gollem/provider/openai"
	"github.com/snabb/webostv"
)

//go:embed warning.mp3
var warningMP3 []byte

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type Config struct {
	SonosIP          string             `json:"sonos_ip"`
	LGTVIP           string             `json:"lg_tv_ip"`
	LGTVKey          string             `json:"lg_tv_key"`
	WarningDelaySecs int                `json:"warning_delay_seconds"`
	LoungeAuth       *auth.AuthStateData `json:"lounge_auth,omitempty"`
}

var defaultConfig = Config{
	SonosIP:          "",
	LGTVIP:           "",
	LGTVKey:          "",
	WarningDelaySecs: 10,
}

func configPath() string {
	// Check working directory first, then fall back to executable directory
	if _, err := os.Stat("config.json"); err == nil {
		abs, _ := filepath.Abs("config.json")
		return abs
	}
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func loadConfig() Config {
	cfg := defaultConfig
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func saveConfig(cfg Config) {
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(configPath(), data, 0644)
}

// ---------------------------------------------------------------------------
// LLM-based brainrot classification
// ---------------------------------------------------------------------------

// BrainrotClassification is the structured output from the LLM.
type BrainrotClassification struct {
	IsBrainrot bool   `json:"is_brainrot" jsonschema:"description=Whether the video is brainrot content"`
	Confidence string `json:"confidence" jsonschema:"description=How confident the classification is,enum=high|medium|low"`
	Reason     string `json:"reason" jsonschema:"description=Brief explanation of why this is or is not brainrot"`
}

func newClassifier() (*core.Agent[BrainrotClassification], error) {
	creds, err := openaiauth.LoadCredentials()
	if err != nil {
		slog.Info("No saved ChatGPT credentials. Starting OAuth login...")
		creds, err = openaiauth.Login(context.Background(), openaiauth.LoginConfig{})
		if err != nil {
			return nil, fmt.Errorf("oauth login failed: %w", err)
		}
		if err := openaiauth.SaveCredentials(creds); err != nil {
			slog.Warn("Failed to save credentials", "error", err)
		}
	}

	creds, err = openaiauth.RefreshIfNeeded(creds)
	if err != nil {
		slog.Warn("Token refresh failed, using existing token", "error", err)
	} else {
		_ = openaiauth.SaveCredentials(creds)
	}

	provider := openaiprovider.New(
		openaiprovider.WithChatGPTAuth(creds.AccessToken, creds.AccountID),
		openaiprovider.WithModel("gpt-5.4"),
		openaiprovider.WithMaxTokens(256),
		openaiprovider.WithPromptCacheKey("brainrot-classifier"),
		openaiprovider.WithTokenRefresher(func() (string, error) {
			refreshed, err := openaiauth.RefreshIfNeeded(creds)
			if err != nil {
				return creds.AccessToken, nil
			}
			creds = refreshed
			_ = openaiauth.SaveCredentials(creds)
			return creds.AccessToken, nil
		}),
	)

	agent := core.NewAgent[BrainrotClassification](provider,
		core.WithSystemPrompt[BrainrotClassification](`You are a parental content filter for a 6-year-old child's YouTube viewing. You classify videos as harmful ("brainrot") or acceptable.

The goal is NOT to block all screen time — it is to block low-quality, manipulative, overstimulating junk while allowing genuinely good content for a young child.

FLAG AS BRAINROT (is_brainrot=true):
- YouTube Shorts (any short-form vertical video, typically <=60s) — the format itself is addictive
- TikTok compilations or reposts
- Roblox clickbait: Blox Fruits, Brookhaven drama, "BANNED" / "HACKER" videos
- Minecraft clickbait/drama (e.g. "BANNED", "trolling" videos) — NOT calm building or tutorial content
- Low-effort gaming clickbait: ALL CAPS titles, excessive emojis, fake drama
- Skibidi toilet, sigma, rizz, gyatt, fanum tax, ohio meme content
- Satisfying/oddly satisfying compilations
- Reaction videos and commentary drama
- MrBeast-style challenge/stunt/extreme content
- Content with titles designed to manipulate: fake urgency, outrage bait, ALL CAPS, excessive emojis
- Content in any language that matches these patterns
- Gacha Life, Elsagate-style content, "story time" animated drama
- Among Us, Poppy Playtime, FNAF clickbait aimed at kids
- Compilation channels, clip channels, "best moments" highlight reels
- Toy unboxing/surprise egg channels that are pure consumption content
- Loud, chaotic "family vlog" channels (e.g. FGTeeV-style screaming content)
- Content that is clearly age-inappropriate (violence, horror, crude humor)

ALLOW (is_brainrot=false):
- Quality children's shows: Bluey, Puffin Rock, Numberblocks, Octonauts, Magic School Bus, Wild Kratts, Sesame Street, Daniel Tiger, Peppa Pig, Hey Duggee, Storybots, Ask the Storybots
- Educational content: science experiments, nature/animal videos, art/drawing tutorials, reading/phonics, math for kids
- Calm Minecraft: building tutorials, survival let's plays without clickbait titles, redstone guides
- Music: children's songs, nursery rhymes, Raffi, They Might Be Giants, Caspar Babypants, movie/show soundtracks
- Creative content: drawing tutorials, craft/maker videos, LEGO building guides, cooking with kids
- Nature documentaries and animal content (National Geographic Kids, BBC Earth, etc.)
- Family movies, Pixar/Disney/Ghibli clips and trailers
- Genuine sports content
- PBS Kids, Khan Academy Kids, Crash Course Kids, SciShow Kids
- Read-aloud books and audiobook content

When in doubt, FLAG IT. It is better to be overprotective than to let garbage through.

You will be given a video title, channel name, duration, and description/tags. Classify it.`),
	)

	return agent, nil
}

func classifyVideo(ctx context.Context, agent *core.Agent[BrainrotClassification], title, channel string, durationSecs float64, tags []string) (*BrainrotClassification, error) {
	prompt := fmt.Sprintf(
		"Title: %s\nChannel: %s\nDuration: %.0f seconds\nTags: %s",
		title, channel, durationSecs, strings.Join(tags, ", "),
	)
	result, err := agent.Run(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return &result.Output, nil
}

// ---------------------------------------------------------------------------
// YouTube video metadata (no API key needed — uses oEmbed + noembed)
// ---------------------------------------------------------------------------

type videoMeta struct {
	Title       string
	Channel     string
	Description string
}

var metaClient = &http.Client{Timeout: 10 * time.Second}

func fetchVideoMeta(videoID string) (*videoMeta, error) {
	// Fetch the YouTube watch page and extract meta tags.
	// This gives us title, channel, and description with no API key.
	url := fmt.Sprintf("https://www.youtube.com/watch?v=%s", videoID)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := metaClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	page := string(body)

	meta := &videoMeta{}
	meta.Title = extractMeta(page, `<meta name="title" content="`)
	meta.Channel = extractMeta(page, `<link itemprop="name" content="`)
	meta.Description = extractMeta(page, `<meta name="description" content="`)

	// Fallback: try og:title
	if meta.Title == "" {
		meta.Title = extractMeta(page, `<meta property="og:title" content="`)
	}

	if meta.Title == "" {
		return nil, fmt.Errorf("could not extract metadata for %s", videoID)
	}
	return meta, nil
}

func extractMeta(page, prefix string) string {
	idx := strings.Index(page, prefix)
	if idx == -1 {
		return ""
	}
	start := idx + len(prefix)
	end := strings.Index(page[start:], `"`)
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(page[start : start+end])
}

// ---------------------------------------------------------------------------
// Sonos control via direct UPnP/SOAP (no external dependency)
// ---------------------------------------------------------------------------

func sonosSOAP(sonosIP, service, action, body string) (string, error) {
	endpoint := fmt.Sprintf("http://%s:1400/MediaRenderer/%s/Control", sonosIP, service)
	soapAction := fmt.Sprintf("urn:schemas-upnp-org:service:%s:1#%s", service, action)

	envelope := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"
 s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body><u:%s xmlns:u="urn:schemas-upnp-org:service:%s:1">%s</u:%s></s:Body>
</s:Envelope>`, action, service, body, action)

	req, _ := http.NewRequest("POST", endpoint, bytes.NewBufferString(envelope))
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", soapAction)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data), nil
}

func sonosGetVolume(sonosIP string) (int, error) {
	body := `<InstanceID>0</InstanceID><Channel>Master</Channel>`
	resp, err := sonosSOAP(sonosIP, "RenderingControl", "GetVolume", body)
	if err != nil {
		return 0, err
	}
	var env struct {
		Body struct {
			Resp struct {
				Volume int `xml:"CurrentVolume"`
			} `xml:"GetVolumeResponse"`
		}
	}
	if err := xml.Unmarshal([]byte(resp), &env); err != nil {
		return 0, err
	}
	return env.Body.Resp.Volume, nil
}

func sonosSetVolume(sonosIP string, vol int) error {
	body := fmt.Sprintf(`<InstanceID>0</InstanceID><Channel>Master</Channel><DesiredVolume>%d</DesiredVolume>`, vol)
	_, err := sonosSOAP(sonosIP, "RenderingControl", "SetVolume", body)
	return err
}

type sonosMediaInfo struct {
	CurrentURI         string
	CurrentURIMetaData string
}

func sonosGetMediaInfo(sonosIP string) (*sonosMediaInfo, error) {
	resp, err := sonosSOAP(sonosIP, "AVTransport", "GetMediaInfo", `<InstanceID>0</InstanceID>`)
	if err != nil {
		return nil, err
	}
	var env struct {
		Body struct {
			Resp struct {
				CurrentURI         string
				CurrentURIMetaData string
			} `xml:"GetMediaInfoResponse"`
		}
	}
	if err := xml.Unmarshal([]byte(resp), &env); err != nil {
		return nil, err
	}
	return &sonosMediaInfo{
		CurrentURI:         env.Body.Resp.CurrentURI,
		CurrentURIMetaData: env.Body.Resp.CurrentURIMetaData,
	}, nil
}

func sonosGetTransportState(sonosIP string) (string, error) {
	resp, err := sonosSOAP(sonosIP, "AVTransport", "GetTransportInfo", `<InstanceID>0</InstanceID>`)
	if err != nil {
		return "", err
	}
	var env struct {
		Body struct {
			Resp struct {
				State string `xml:"CurrentTransportState"`
			} `xml:"GetTransportInfoResponse"`
		}
	}
	if err := xml.Unmarshal([]byte(resp), &env); err != nil {
		return "", err
	}
	return env.Body.Resp.State, nil
}

func sonosSetAVTransportURI(sonosIP, uri, metadata string) error {
	// XML-escape the metadata since it goes inside SOAP XML
	var metaBuf bytes.Buffer
	xml.EscapeText(&metaBuf, []byte(metadata))
	body := fmt.Sprintf(`<InstanceID>0</InstanceID><CurrentURI>%s</CurrentURI><CurrentURIMetaData>%s</CurrentURIMetaData>`,
		uri, metaBuf.String())
	_, err := sonosSOAP(sonosIP, "AVTransport", "SetAVTransportURI", body)
	return err
}

func sonosPlay(sonosIP string) error {
	_, err := sonosSOAP(sonosIP, "AVTransport", "Play", `<InstanceID>0</InstanceID><Speed>1</Speed>`)
	return err
}

// ---------------------------------------------------------------------------
// Warning playback
// ---------------------------------------------------------------------------

var (
	httpPort       = 8769
	httpServerOnce sync.Once
)

func startHTTPServer() {
	httpServerOnce.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/warning.mp3", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(warningMP3)
		})
		go func() {
			if err := http.ListenAndServe(fmt.Sprintf(":%d", httpPort), mux); err != nil {
				log.Fatalf("Warning audio server failed to start on port %d: %v", httpPort, err)
			}
		}()
	})
}

func getLocalIP() string {
	conn, err := net.Dial("udp", "192.168.0.1:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func playWarningOnSonos(sonosIP string) error {
	slog.Info("Playing warning on Sonos")
	localIP := getLocalIP()
	audioURL := fmt.Sprintf("http://%s:%d/warning.mp3", localIP, httpPort)

	// Capture current state
	vol, err := sonosGetVolume(sonosIP)
	if err != nil {
		slog.Warn("Could not get volume", "error", err)
		vol = 15
	}
	mediaInfo, _ := sonosGetMediaInfo(sonosIP)
	transportState, _ := sonosGetTransportState(sonosIP)

	// Bump volume 30% for warning
	warningVol := min(100, int(float64(vol)*1.3))
	slog.Info("Volume", "current", vol, "warning", warningVol)
	sonosSetVolume(sonosIP, warningVol)

	// Play warning
	sonosSetAVTransportURI(sonosIP, audioURL, "")
	sonosPlay(sonosIP)

	// Wait for warning to finish (~8 seconds)
	time.Sleep(9 * time.Second)

	// Restore original volume and input
	sonosSetVolume(sonosIP, vol)
	if mediaInfo != nil && mediaInfo.CurrentURI != "" {
		sonosSetAVTransportURI(sonosIP, mediaInfo.CurrentURI, mediaInfo.CurrentURIMetaData)
		if transportState == "PLAYING" {
			sonosPlay(sonosIP)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// LG TV control — persistent connection with background keepalive
// ---------------------------------------------------------------------------

type tvController struct {
	ip        string
	clientKey string
	tv        *webostv.Tv
	mu        sync.Mutex
}

func newTVController(ip, clientKey string) *tvController {
	return &tvController{ip: ip, clientKey: clientKey}
}

// connect dials and registers with the TV. Caller must hold tc.mu.
func (tc *tvController) connect() error {
	if tc.tv != nil {
		tc.tv.Close()
		tc.tv = nil
	}
	tv, err := webostv.DefaultDialer.Dial(tc.ip)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	go tv.MessageHandler()

	if _, err := tv.Register(tc.clientKey); err != nil {
		tv.Close()
		return fmt.Errorf("register: %w", err)
	}
	tc.tv = tv
	return nil
}

// keepAlive runs in the background, maintaining a connection to the TV.
// It reconnects every 30 seconds if the connection is lost.
func (tc *tvController) keepAlive(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	wasConnected := false

	// Initial connection
	tc.mu.Lock()
	if err := tc.connect(); err != nil {
		slog.Debug("TV not reachable yet", "error", err)
	} else {
		slog.Info("TV connection established")
		tc.tv.SystemNotificationsCreateToast("Brainrot Detection is now active.")
		wasConnected = true
	}
	tc.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			tc.mu.Lock()
			if tc.tv != nil {
				tc.tv.SystemNotificationsCreateToast("Brainrot Detection is shutting down.")
				time.Sleep(500 * time.Millisecond) // let the toast send before closing
				tc.tv.Close()
				tc.tv = nil
			}
			tc.mu.Unlock()
			return
		case <-ticker.C:
			tc.mu.Lock()
			// Test the connection by getting foreground app info
			if tc.tv != nil {
				_, err := tc.tv.ApplicationManagerGetForegroundAppInfo()
				if err != nil {
					slog.Debug("TV connection stale, reconnecting", "error", err)
					tc.tv.Close()
					tc.tv = nil
					wasConnected = false
				}
			}
			if tc.tv == nil {
				if err := tc.connect(); err != nil {
					slog.Debug("TV reconnect failed", "error", err)
				} else {
					if !wasConnected {
						slog.Info("TV reconnected")
						tc.tv.SystemNotificationsCreateToast("Brainrot Detection reconnected.")
					}
					wasConnected = true
				}
			}
			tc.mu.Unlock()
		}
	}
}

func (tc *tvController) turnOff() error {
	slog.Warn("TURNING OFF THE TV")
	tc.mu.Lock()
	defer tc.mu.Unlock()

	for attempt := range 3 {
		// Use existing connection or establish a new one
		if tc.tv == nil {
			if err := tc.connect(); err != nil {
				slog.Warn("TV dial failed, retrying", "attempt", attempt+1, "error", err)
				time.Sleep(2 * time.Second)
				continue
			}
		}

		err := tc.tv.SystemTurnOff()
		if err == nil {
			slog.Info("TV has been turned off.")
			tc.tv.Close()
			tc.tv = nil
			return nil
		}

		slog.Warn("TV power-off failed, retrying", "attempt", attempt+1, "error", err)
		tc.tv.Close()
		tc.tv = nil
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("failed to turn off TV after 3 attempts")
}

func (tc *tvController) showToast(msg string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.tv == nil {
		return
	}
	if _, err := tc.tv.SystemNotificationsCreateToast(msg); err != nil {
		slog.Debug("Toast failed", "error", err)
	}
}

// ---------------------------------------------------------------------------
// YouTube Lounge monitoring
// ---------------------------------------------------------------------------

type brainrotMonitor struct {
	cfg           Config
	classifier    *core.Agent[BrainrotClassification]
	tv            *tvController
	lastVideoID   string             // most recent video we saw
	activeWarning bool               // a warning/shutdown sequence is in progress
	warningCancel context.CancelFunc // cancels the warning countdown
	warningDone   chan struct{}       // closed when warnAndShutdown goroutine exits
	mu            sync.Mutex
}

func (m *brainrotMonitor) handleNowPlaying(ctx context.Context, e *event.NowPlayingEvent) {
	// Ignore empty — transient Lounge event (buffering, seeking, pausing).
	if e.VideoID == "" {
		return
	}

	// Check if this is a new video (lock briefly, don't hold during network calls)
	m.mu.Lock()
	if e.VideoID == m.lastVideoID {
		m.mu.Unlock()
		return
	}
	m.lastVideoID = e.VideoID
	m.mu.Unlock()

	var duration float64
	if e.Duration != nil {
		duration = *e.Duration
	}

	// Fetch actual video metadata from YouTube (no mutex held)
	title, channel, description := e.VideoID, "unknown", ""
	meta, err := fetchVideoMeta(e.VideoID)
	if err != nil {
		slog.Warn("Could not fetch video metadata", "video_id", e.VideoID, "error", err)
	} else {
		title = meta.Title
		channel = meta.Channel
		description = meta.Description
	}

	slog.Info("Now playing",
		"video_id", e.VideoID,
		"title", title,
		"channel", channel,
		"duration", duration,
		"state", e.State,
	)

	// Use LLM to classify with real metadata (no mutex held — this takes seconds)
	classCtx, classCancel := context.WithTimeout(ctx, 30*time.Second)
	classification, classErr := classifyVideo(classCtx, m.classifier,
		title,
		channel,
		duration,
		[]string{description},
	)
	classCancel()

	isShort := duration > 0 && duration <= 61

	if classErr != nil {
		slog.Error("Classification failed", "error", classErr)
		if !isShort {
			return
		}
		classification = &BrainrotClassification{
			IsBrainrot: true,
			Confidence: "low",
			Reason:     fmt.Sprintf("YouTube Short (%.0fs), classification unavailable", duration),
		}
	}

	// Now take the lock to update state
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check that this video is still the current one (could have changed during classification)
	if e.VideoID != m.lastVideoID {
		return
	}

	if classification.IsBrainrot {
		slog.Warn("BRAINROT DETECTED",
			"video_id", e.VideoID,
			"title", title,
			"confidence", classification.Confidence,
			"reason", classification.Reason,
		)

		// Start warning if not already in a warning sequence
		if !m.activeWarning {
			// If a previous goroutine is still finishing, wait for it
			m.waitForWarningDone()
			m.activeWarning = true
			warnCtx, cancel := context.WithCancel(ctx)
			m.warningCancel = cancel
			m.warningDone = make(chan struct{})
			go m.warnAndShutdown(warnCtx)
		}
	} else {
		slog.Info("Content OK",
			"video_id", e.VideoID,
			"title", title,
			"reason", classification.Reason,
		)
		// Only cancel an active warning if we see confirmed non-brainrot content.
		if m.activeWarning {
			slog.Info("Non-brainrot content confirmed — cancelling warning")
			m.stopWarning()
		}
	}
}

func (m *brainrotMonitor) warnAndShutdown(ctx context.Context) {
	defer close(m.warningDone)

	// Show visual warning on TV screen
	m.tv.showToast("BRAINROT DETECTED. Change what you're watching or the TV will turn off in 10 seconds.")

	// Play warning audio — this is not cancellable, it always plays through
	if err := playWarningOnSonos(m.cfg.SonosIP); err != nil {
		slog.Error("Failed to play warning", "error", err)
	}

	// Countdown — this CAN be cancelled if she switches to non-brainrot content
	slog.Info("Countdown started", "seconds", m.cfg.WarningDelaySecs)
	select {
	case <-ctx.Done():
		slog.Info("Warning cancelled — non-brainrot content detected")
		return
	case <-time.After(time.Duration(m.cfg.WarningDelaySecs) * time.Second):
	}

	// Check if we were cancelled right at the deadline (race between timer and cancel)
	select {
	case <-ctx.Done():
		slog.Info("Warning cancelled — non-brainrot content detected")
		return
	default:
	}

	// Final check — context could have been cancelled between timer firing and here
	m.mu.Lock()
	if !m.activeWarning {
		m.mu.Unlock()
		slog.Info("Warning was cancelled just in time")
		return
	}
	m.activeWarning = false
	m.mu.Unlock()

	// Time's up. Turn off the TV.
	slog.Warn("Time's up. Turning off TV.")
	if err := m.tv.turnOff(); err != nil {
		slog.Error("Failed to turn off TV", "error", err)
	}
}

// stopWarning cancels the active warning and waits for the goroutine to exit.
// Caller must hold m.mu.
func (m *brainrotMonitor) stopWarning() {
	if m.warningCancel != nil {
		m.warningCancel()
		m.warningCancel = nil
	}
	m.activeWarning = false
	// Release lock while waiting so the goroutine can acquire it to clean up
	done := m.warningDone
	m.mu.Unlock()
	if done != nil {
		<-done
	}
	m.mu.Lock()
}

// waitForWarningDone waits for a previous warning goroutine to finish.
// Caller must hold m.mu.
func (m *brainrotMonitor) waitForWarningDone() {
	done := m.warningDone
	if done == nil {
		return
	}
	m.mu.Unlock()
	<-done
	m.mu.Lock()
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := loadConfig()

	if cfg.SonosIP == "" || cfg.LGTVIP == "" {
		log.Fatalf("Config incomplete: sonos_ip and lg_tv_ip are required.\nCopy config.sample.json to config.json and fill in your device IPs.")
	}

	slog.Info("Initializing brainrot classifier...")
	classifier, err := newClassifier()
	if err != nil {
		log.Fatalf("Failed to initialize classifier: %v", err)
	}

	startHTTPServer()
	slog.Info("Warning audio server started", "port", httpPort)

	client := lounge.NewClient("BrainrotDetector")

	if cfg.LoungeAuth == nil {
		fmt.Println("\n============================================================")
		fmt.Println("  PS5 YouTube Pairing")
		fmt.Println("============================================================")
		fmt.Println("\nOn your PS5:")
		fmt.Println("  1. Open the YouTube app")
		fmt.Println("  2. Go to Settings (left sidebar)")
		fmt.Println("  3. Select 'Link with TV code'")
		fmt.Println("  4. Enter the 12-digit code below")
		fmt.Print("\nEnter the TV code: ")

		var code string
		fmt.Scanln(&code)
		code = strings.ReplaceAll(code, " ", "")

		pairCtx, pairCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer pairCancel()

		if err := client.Pair(pairCtx, code); err != nil {
			log.Fatalf("Pairing failed: %v", err)
		}

		slog.Info("Paired successfully", "screen", client.ScreenName())
		authState := client.StoreAuthState()
		cfg.LoungeAuth = &authState
		saveConfig(cfg)
	} else {
		client.LoadAuthState(*cfg.LoungeAuth)

		refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer refreshCancel()
		if err := client.RefreshAuth(refreshCtx); err != nil {
			slog.Warn("Auth refresh failed, may need to re-pair", "error", err)
		} else {
			authState := client.StoreAuthState()
			cfg.LoungeAuth = &authState
			saveConfig(cfg)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	tvCtrl := newTVController(cfg.LGTVIP, cfg.LGTVKey)
	go tvCtrl.keepAlive(ctx)

	monitor := &brainrotMonitor{
		cfg:          cfg,
		classifier:   classifier,
		tv:           tvCtrl,
	}

	// Reconnect loop — when the TV turns off or YouTube closes, the Lounge
	// session ends and the event channel closes. We refresh auth, reconnect,
	// and re-subscribe automatically.
	for {
		slog.Info("Connecting to YouTube Lounge (PS5)...")

		// Refresh token before each connection attempt
		refreshCtx, refreshCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := client.RefreshAuth(refreshCtx); err != nil {
			slog.Warn("Auth refresh failed", "error", err)
		} else {
			authState := client.StoreAuthState()
			cfg.LoungeAuth = &authState
			saveConfig(cfg)
		}
		refreshCancel()

		if err := client.Connect(ctx); err != nil {
			slog.Error("Failed to connect", "error", err)
			// Wait before retrying — YouTube app might not be open yet
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		slog.Info("Brainrot Detection Agent is ACTIVE",
			"sonos", cfg.SonosIP,
			"tv", cfg.LGTVIP,
			"warning_delay", cfg.WarningDelaySecs,
		)

		events, err := client.Subscribe(ctx)
		if err != nil {
			slog.Error("Failed to subscribe", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		// Process events until the channel closes (session ended)
		for ev := range events {
			if e, ok := ev.(*event.NowPlayingEvent); ok {
				monitor.handleNowPlaying(ctx, e)
			}
		}

		// Event channel closed — session is dead. Reset warning state and reconnect.
		slog.Warn("Lounge session ended — will reconnect in 5 seconds...")
		monitor.mu.Lock()
		monitor.lastVideoID = ""
		if monitor.activeWarning {
			monitor.stopWarning()
		}
		monitor.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
