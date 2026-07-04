package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var allowedApps = map[string]bool{"rtp": true, "udp": true}

type streamLimitError struct {
	message string
}

func (e streamLimitError) Error() string {
	return e.message
}

type streamID struct {
	app    string
	stream string
}

func (s streamID) key() string {
	return s.app + "/" + s.stream
}

type udpOptions struct {
	fifoSize  string
	timeoutUS string
	localAddr string
}

type ffmpegOptions struct {
	ffmpegBin    string
	logLevel     string
	inputURL     string
	outputURL    string
	audioCodec   string
	audioBitrate string
}

type streamRegistry struct {
	maxStreams int
	watchers   map[streamID]map[string]struct{}
	mu         sync.Mutex
}

func newStreamRegistry(maxStreams int) *streamRegistry {
	return &streamRegistry{
		maxStreams: maxStreams,
		watchers:   map[streamID]map[string]struct{}{},
	}
}

func (r *streamRegistry) reserve(app, stream, clientID string) (bool, error) {
	id := streamID{app: app, stream: stream}
	r.mu.Lock()
	defer r.mu.Unlock()

	if clients, ok := r.watchers[id]; ok {
		clients[clientID] = struct{}{}
		return false, nil
	}
	if len(r.watchers) >= r.maxStreams {
		return false, streamLimitError{message: fmt.Sprintf("too many distinct streams: max=%d", r.maxStreams)}
	}
	r.watchers[id] = map[string]struct{}{clientID: {}}
	return true, nil
}

func (r *streamRegistry) release(app, stream, clientID string) bool {
	id := streamID{app: app, stream: stream}
	r.mu.Lock()
	defer r.mu.Unlock()

	clients, ok := r.watchers[id]
	if !ok {
		return true
	}
	delete(clients, clientID)
	if len(clients) > 0 {
		return false
	}
	delete(r.watchers, id)
	return true
}

func (r *streamRegistry) clear(id streamID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.watchers, id)
}

func (r *streamRegistry) has(id streamID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.watchers[id]
	return ok
}

func (r *streamRegistry) activeStreamCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.watchers)
}

func (r *streamRegistry) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := map[string]int{}
	for id, clients := range r.watchers {
		result[id.key()] = len(clients)
	}
	return result
}

type processEntry struct {
	cmd *exec.Cmd
}

type puller struct {
	registry          *streamRegistry
	udpxyBaseURL     string
	srsRTMPBaseURL   string
	ffmpegBin        string
	ffprobeBin       string
	ffmpegLogLevel   string
	audioBitrate     string
	ffprobeTimeout   time.Duration
	idleStopDuration time.Duration
	udpOptions       udpOptions
	mu               sync.Mutex
	processes        map[streamID]processEntry
	starting         map[streamID]struct{}
	stopTimers       map[streamID]*time.Timer
}

type hookPayload struct {
	App      string      `json:"app"`
	Stream   string      `json:"stream"`
	ClientID interface{} `json:"client_id"`
	IP       string      `json:"ip"`
	TCURL    string      `json:"tcUrl"`
}

func newPullerFromEnv() *puller {
	return &puller{
		registry:          newStreamRegistry(envInt("MAX_STREAMS", 3)),
		udpxyBaseURL:     os.Getenv("UDPXY_BASE_URL"),
		srsRTMPBaseURL:   strings.TrimRight(strings.TrimSpace(os.Getenv("SRS_RTMP_BASE_URL")), "/"),
		ffmpegBin:        envString("FFMPEG_BIN", "ffmpeg"),
		ffprobeBin:       envString("FFPROBE_BIN", "ffprobe"),
		ffmpegLogLevel:   envString("FFMPEG_LOGLEVEL", "warning"),
		audioBitrate:     envString("AUDIO_BITRATE", "128k"),
		ffprobeTimeout:   time.Duration(envInt("FFPROBE_TIMEOUT_SEC", 8)) * time.Second,
		idleStopDuration: time.Duration(envInt("IDLE_STOP_SEC", 5)) * time.Second,
		udpOptions: udpOptions{
			fifoSize:  envString("UDP_FIFO_SIZE", "1000000"),
			timeoutUS: envString("UDP_TIMEOUT_US", "5000000"),
			localAddr: strings.TrimSpace(os.Getenv("UDP_LOCALADDR")),
		},
		processes:  map[streamID]processEntry{},
		starting:   map[streamID]struct{}{},
		stopTimers: map[streamID]*time.Timer{},
	}
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseStream(stream string) (net.IP, int, error) {
	host, portText, ok := splitStream(stream)
	if !ok {
		return nil, 0, fmt.Errorf("stream must be formatted as <ip_a>_<ip_b>_<ip_c>_<ip_d>_<port>: %s", stream)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return nil, 0, fmt.Errorf("invalid IPv4 address: %s", host)
	}
	if ip[0] < 224 || ip[0] > 239 {
		return nil, 0, fmt.Errorf("not a multicast address: %s", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, 0, fmt.Errorf("UDP port out of range: %s", portText)
	}
	return ip, port, nil
}

func splitStream(stream string) (string, string, bool) {
	parts := strings.Split(stream, "_")
	if len(parts) == 5 {
		return strings.Join(parts[:4], "."), parts[4], true
	}
	return "", "", false
}

func normalizeUDPXYBase(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	return baseURL
}

func buildInputURL(app, stream, udpxyBaseURL string, options udpOptions) (string, error) {
	if !allowedApps[app] {
		return "", fmt.Errorf("unsupported app: %s", app)
	}
	ip, port, err := parseStream(stream)
	if err != nil {
		return "", err
	}
	ipText := ip.String()
	baseURL := normalizeUDPXYBase(udpxyBaseURL)
	if baseURL != "" {
		return fmt.Sprintf("%s/%s/%s:%d", baseURL, app, ipText, port), nil
	}
	if app == "rtp" {
		return fmt.Sprintf("rtp://@%s:%d", ipText, port), nil
	}
	fifoSize := options.fifoSize
	if fifoSize == "" {
		fifoSize = "1000000"
	}
	timeoutUS := options.timeoutUS
	if timeoutUS == "" {
		timeoutUS = "5000000"
	}
	query := fmt.Sprintf("fifo_size=%s&overrun_nonfatal=1&timeout=%s", fifoSize, timeoutUS)
	if options.localAddr != "" {
		query += "&localaddr=" + options.localAddr
	}
	return fmt.Sprintf("udp://@%s:%d?%s", ipText, port, query), nil
}

func buildOutputURL(baseURL, app, stream string) string {
	return strings.TrimRight(baseURL, "/") + "/" + app + "/" + stream
}

func buildPublishOutputURL(configuredBaseURL, tcURL, app, stream string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(configuredBaseURL), "/")
	if strings.EqualFold(baseURL, "auto") {
		baseURL = ""
	}
	if baseURL == "" {
		baseURL = normalizeHookTCURL(tcURL, app)
	}
	if baseURL == "" {
		baseURL = "rtmp://127.0.0.1:1935"
	}
	return buildOutputURL(baseURL, app, stream)
}

func normalizeHookTCURL(tcURL, app string) string {
	parsed, err := url.Parse(strings.TrimSpace(tcURL))
	if err != nil || parsed.Scheme != "rtmp" || parsed.Host == "" {
		return ""
	}
	if strings.Trim(strings.TrimSpace(parsed.Path), "/") != app {
		return ""
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func buildFFmpegCommand(options ffmpegOptions) []string {
	command := []string{
		options.ffmpegBin,
		"-hide_banner",
		"-loglevel",
		options.logLevel,
		"-i",
		options.inputURL,
		"-c:v",
		"copy",
	}
	if options.audioCodec == "aac" {
		command = append(command, "-c:a", "copy")
	} else {
		command = append(command, "-c:a", "aac", "-b:a", options.audioBitrate, "-ar", "48000", "-ac", "2")
	}
	return append(command, "-f", "flv", options.outputURL)
}

func (p *puller) probeAudioCodec(inputURL string) string {
	ctx, cancel := context.WithTimeout(context.Background(), p.ffprobeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, p.ffprobeBin,
		"-v", "quiet",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputURL,
	)
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		log.Printf("ffprobe failed input=%s error=%v", inputURL, err)
		return ""
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

func (p *puller) onPlay(payload hookPayload) (int, string) {
	app, stream, clientID, err := readPayload(payload)
	if err != nil {
		return 1, err.Error()
	}
	if _, err := p.registry.reserve(app, stream, clientID); err != nil {
		log.Printf("reject play app=%s stream=%s client=%s error=%v", app, stream, clientID, err)
		return 1, err.Error()
	}

	id := streamID{app: app, stream: stream}
	p.mu.Lock()
	if timer := p.stopTimers[id]; timer != nil {
		timer.Stop()
		delete(p.stopTimers, id)
	}
	if _, ok := p.starting[id]; ok {
		p.mu.Unlock()
		log.Printf("reuse starting key=%s client=%s", id.key(), clientID)
		return 0, "ok"
	}
	if entry, ok := p.processes[id]; ok && entry.cmd.Process != nil && entry.cmd.ProcessState == nil {
		p.mu.Unlock()
		log.Printf("reuse key=%s client=%s", id.key(), clientID)
		return 0, "ok"
	}
	p.starting[id] = struct{}{}
	p.mu.Unlock()

	go func() {
		if err := p.startProcess(id, payload.TCURL); err != nil {
			p.registry.clear(id)
			log.Printf("failed to start key=%s error=%v", id.key(), err)
		}
	}()
	return 0, "ok"
}

func (p *puller) onStop(payload hookPayload) (int, string) {
	app, stream, clientID, err := readPayload(payload)
	if err != nil {
		return 0, "ok"
	}
	if p.registry.release(app, stream, clientID) {
		p.scheduleStop(streamID{app: app, stream: stream})
	}
	return 0, "ok"
}

func readPayload(payload hookPayload) (string, string, string, error) {
	app := strings.TrimSpace(payload.App)
	stream := strings.TrimSpace(payload.Stream)
	if !allowedApps[app] {
		return "", "", "", fmt.Errorf("unsupported app: %s", app)
	}
	if _, _, err := parseStream(stream); err != nil {
		return "", "", "", err
	}
	clientID := strings.TrimSpace(fmt.Sprint(payload.ClientID))
	if clientID == "" || clientID == "<nil>" {
		clientID = strings.TrimSpace(payload.IP)
	}
	if clientID == "" {
		clientID = "unknown"
	}
	return app, stream, clientID, nil
}

func (p *puller) startProcess(id streamID, tcURL string) error {
	started := false
	defer func() {
		if !started {
			p.clearStarting(id)
		}
	}()

	inputURL, err := buildInputURL(id.app, id.stream, p.udpxyBaseURL, p.udpOptions)
	if err != nil {
		return err
	}
	outputURL := buildPublishOutputURL(p.srsRTMPBaseURL, tcURL, id.app, id.stream)
	audioCodec := p.probeAudioCodec(inputURL)
	command := buildFFmpegCommand(ffmpegOptions{
		ffmpegBin:    p.ffmpegBin,
		logLevel:     p.ffmpegLogLevel,
		inputURL:     inputURL,
		outputURL:    outputURL,
		audioCodec:   audioCodec,
		audioBitrate: p.audioBitrate,
	})
	log.Printf("start key=%s input=%s audio_codec=%s output=%s", id.key(), inputURL, fallback(audioCodec, "unknown"), outputURL)
	if !p.registry.has(id) {
		log.Printf("skip start key=%s no watchers", id.key())
		return nil
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.starting, id)
	p.processes[id] = processEntry{cmd: cmd}
	p.mu.Unlock()
	started = true
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		current, ok := p.processes[id]
		if ok && current.cmd == cmd {
			delete(p.processes, id)
		}
		delete(p.starting, id)
		if timer := p.stopTimers[id]; timer != nil {
			timer.Stop()
			delete(p.stopTimers, id)
		}
		p.mu.Unlock()
		p.registry.clear(id)
		if err != nil {
			log.Printf("ffmpeg exited key=%s error=%v", id.key(), err)
		} else {
			log.Printf("ffmpeg exited key=%s", id.key())
		}
	}()
	return nil
}

func (p *puller) clearStarting(id streamID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.starting, id)
}

func fallback(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

func (p *puller) scheduleStop(id streamID) {
	timer := time.AfterFunc(p.idleStopDuration, func() {
		p.stopProcess(id)
	})
	p.mu.Lock()
	if old := p.stopTimers[id]; old != nil {
		old.Stop()
	}
	p.stopTimers[id] = timer
	p.mu.Unlock()
	log.Printf("schedule stop key=%s in=%s", id.key(), p.idleStopDuration)
}

func (p *puller) stopProcess(id streamID) {
	p.mu.Lock()
	delete(p.stopTimers, id)
	entry, ok := p.processes[id]
	if ok {
		delete(p.processes, id)
	}
	p.mu.Unlock()
	if !ok || entry.cmd.Process == nil {
		return
	}
	log.Printf("stop key=%s pid=%d", id.key(), entry.cmd.Process.Pid)
	if err := entry.cmd.Process.Signal(os.Interrupt); err != nil {
		_ = entry.cmd.Process.Kill()
		return
	}
	time.Sleep(5 * time.Second)
	if entry.cmd.ProcessState == nil {
		_ = entry.cmd.Process.Kill()
	}
}

func (p *puller) shutdown() {
	p.mu.Lock()
	ids := make([]streamID, 0, len(p.processes))
	for id := range p.processes {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		p.stopProcess(id)
	}
}

func (p *puller) streams() map[string]any {
	p.mu.Lock()
	processes := map[string]int{}
	for id, entry := range p.processes {
		if entry.cmd.Process != nil {
			processes[id.key()] = entry.cmd.Process.Pid
		}
	}
	starting := map[string]bool{}
	for id := range p.starting {
		starting[id.key()] = true
	}
	p.mu.Unlock()
	return map[string]any{
		"max_streams":    p.registry.maxStreams,
		"active_streams": p.registry.snapshot(),
		"processes":      processes,
		"starting":       starting,
	}
}

type server struct {
	puller *puller
}

func (s server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writeJSON(w, map[string]bool{"ok": true})
	case r.Method == http.MethodGet && r.URL.Path == "/streams":
		writeJSON(w, s.puller.streams())
	case r.Method == http.MethodPost && r.URL.Path == "/srs/on_play":
		s.handleHook(w, r, s.puller.onPlay)
	case r.Method == http.MethodPost && r.URL.Path == "/srs/on_stop":
		s.handleHook(w, r, s.puller.onStop)
	default:
		http.NotFound(w, r)
	}
}

func (s server) handleHook(w http.ResponseWriter, r *http.Request, fn func(hookPayload) (int, string)) {
	defer r.Body.Close()
	var payload hookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("invalid hook json path=%s error=%v", r.URL.Path, err)
	}
	code, message := fn(payload)
	if code != 0 {
		log.Printf("reject path=%s message=%s", r.URL.Path, message)
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprint(w, code)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func main() {
	p := newPullerFromEnv()
	addr := ":" + envString("PULLER_PORT", "18090")
	srv := &http.Server{Addr: addr, Handler: server{puller: p}}
	log.Printf("listening addr=%s max_streams=%d", addr, p.registry.maxStreams)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}
