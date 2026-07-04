package main

import "testing"

func TestRTPStreamUsesUDPXYRTPPathWhenBaseConfigured(t *testing.T) {
	source, err := buildInputURL("rtp", "239_254_97_96_8550", "192.168.10.1:4000", udpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if source != "http://192.168.10.1:4000/rtp/239.254.97.96:8550" {
		t.Fatalf("source = %q", source)
	}
}

func TestUDPStreamUsesUDPXYUDPPathWhenBaseConfigured(t *testing.T) {
	source, err := buildInputURL("udp", "239_254_97_96_8550", "http://192.168.10.1:4000/", udpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if source != "http://192.168.10.1:4000/udp/239.254.97.96:8550" {
		t.Fatalf("source = %q", source)
	}
}

func TestRTPStreamUsesDirectRTPWhenUDPXYEmpty(t *testing.T) {
	source, err := buildInputURL("rtp", "239_254_97_96_8550", "", udpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if source != "rtp://@239.254.97.96:8550" {
		t.Fatalf("source = %q", source)
	}
}

func TestUDPStreamUsesDirectUDPWhenUDPXYEmpty(t *testing.T) {
	source, err := buildInputURL("udp", "239_254_97_96_8550", "", udpOptions{
		fifoSize:  "1000000",
		timeoutUS: "5000000",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "udp://@239.254.97.96:8550?fifo_size=1000000&overrun_nonfatal=1&timeout=5000000"
	if source != want {
		t.Fatalf("source = %q", source)
	}
}

func TestColonStreamIsRejected(t *testing.T) {
	if _, _, err := parseStream("239.254.97.96:8550"); err == nil {
		t.Fatal("expected colon stream key to be rejected")
	}
}

func TestDotUnderscoreStreamIsRejected(t *testing.T) {
	if _, _, err := parseStream("239.254.97.96_8550"); err == nil {
		t.Fatal("expected dot underscore stream key to be rejected")
	}
}

func TestSRSSafeUnderscoreStreamIsAccepted(t *testing.T) {
	ip, port, err := parseStream("239_254_97_96_8550")
	if err != nil {
		t.Fatal(err)
	}
	if ip.String() != "239.254.97.96" || port != 8550 {
		t.Fatalf("parsed %s:%d", ip, port)
	}
}

func TestSRSSafeUnderscoreStreamBuildsUDPXYURL(t *testing.T) {
	source, err := buildInputURL("udp", "239_254_97_96_8550", "192.168.10.1:4000", udpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if source != "http://192.168.10.1:4000/udp/239.254.97.96:8550" {
		t.Fatalf("source = %q", source)
	}
}

func TestAACAudioIsCopied(t *testing.T) {
	command := buildFFmpegCommand(ffmpegOptions{
		ffmpegBin:    "ffmpeg",
		logLevel:     "warning",
		inputURL:     "http://192.168.10.1:4000/rtp/239.254.97.96:8550",
		outputURL:    "rtmp://127.0.0.1:1935/rtp/239.254.97.96:8550",
		audioCodec:   "aac",
		audioBitrate: "128k",
	})
	if got := valueAfter(command, "-c:a"); got != "copy" {
		t.Fatalf("-c:a = %q", got)
	}
	if got := valueAfter(command, "-c:v"); got != "copy" {
		t.Fatalf("-c:v = %q", got)
	}
}

func TestNonAACAudioIsTranscodedToAAC(t *testing.T) {
	command := buildFFmpegCommand(ffmpegOptions{
		ffmpegBin:    "ffmpeg",
		logLevel:     "warning",
		inputURL:     "http://192.168.10.1:4000/udp/239.254.97.96:8550",
		outputURL:    "rtmp://127.0.0.1:1935/udp/239.254.97.96:8550",
		audioCodec:   "mp2",
		audioBitrate: "128k",
	})
	if got := valueAfter(command, "-c:a"); got != "aac" {
		t.Fatalf("-c:a = %q", got)
	}
	if got := valueAfter(command, "-b:a"); got != "128k" {
		t.Fatalf("-b:a = %q", got)
	}
}

func TestPublishOutputUsesHookTCURLWhenBaseIsAuto(t *testing.T) {
	output := buildPublishOutputURL("auto", "rtmp://192.168.10.20:1935/udp", "udp", "239_254_97_96_8550")
	if output != "rtmp://192.168.10.20:1935/udp/239_254_97_96_8550" {
		t.Fatalf("output = %q", output)
	}
}

func TestPublishOutputUsesConfiguredBaseWhenSet(t *testing.T) {
	output := buildPublishOutputURL("rtmp://127.0.0.1:1935", "rtmp://192.168.10.20:1935/udp", "udp", "239_254_97_96_8550")
	if output != "rtmp://127.0.0.1:1935/udp/239_254_97_96_8550" {
		t.Fatalf("output = %q", output)
	}
}

func TestFourthDistinctStreamIsRejected(t *testing.T) {
	registry := newStreamRegistry(3)
	if _, err := registry.reserve("rtp", "239.0.0.1:1001", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.reserve("rtp", "239.0.0.2:1002", "c2"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.reserve("rtp", "239.0.0.3:1003", "c3"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.reserve("rtp", "239.0.0.4:1004", "c4"); err == nil {
		t.Fatal("expected stream limit error")
	}
}

func TestSameStreamDoesNotConsumeAnotherSlot(t *testing.T) {
	registry := newStreamRegistry(3)
	if _, err := registry.reserve("rtp", "239.0.0.1:1001", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.reserve("rtp", "239.0.0.1:1001", "c2"); err != nil {
		t.Fatal(err)
	}
	if count := registry.activeStreamCount(); count != 1 {
		t.Fatalf("active stream count = %d", count)
	}
}

func TestClearFailedStreamReleasesSlot(t *testing.T) {
	registry := newStreamRegistry(1)
	if _, err := registry.reserve("rtp", "239.0.0.1:1001", "c1"); err != nil {
		t.Fatal(err)
	}
	registry.clear(streamID{app: "rtp", stream: "239.0.0.1:1001"})
	if _, err := registry.reserve("rtp", "239.0.0.2:1002", "c2"); err != nil {
		t.Fatalf("expected released slot, got %v", err)
	}
}

func valueAfter(command []string, flag string) string {
	for i := 0; i < len(command)-1; i++ {
		if command[i] == flag {
			return command[i+1]
		}
	}
	return ""
}
