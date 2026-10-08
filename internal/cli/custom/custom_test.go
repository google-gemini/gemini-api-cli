// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package custom

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/clierrors"
	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
	"github.com/google-gemini/gemini-api-cli/internal/output"
	"github.com/google-gemini/gemini-api-cli/internal/sdk/models/interactions"
	"github.com/spf13/cobra"
)

func TestParseTimestamp(t *testing.T) {
	valid := map[string]int64{
		"7":            7000,
		"7.25":         7250,
		"01:02":        62000,
		"01:02.5":      62500,
		"01:02,500":    62500,
		"1:00:00":      3600000,
		"01:02:03.004": 3723004,
		" 00:05 ":      5000,
		"125.5":        125500,
		"90:15":        5415000,
		"01:02:03,250": 3723250,
	}
	for in, want := range valid {
		got, err := parseTimestamp(in)
		if err != nil || got != want {
			t.Errorf("parseTimestamp(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "1:2:3:4", "-1", "00:-5", "1::2",
		"00:99", "00:60", "1.5:00", "00:00:61", "0:75:00", "1e3", "Inf", "0x10", "00: 05", "5."} {
		if got, err := parseTimestamp(in); err == nil {
			t.Errorf("parseTimestamp(%q) = %d, want an error", in, got)
		}
	}
}

func TestRenderSRT(t *testing.T) {
	segments := []transcriptSegment{
		{Speaker: "Speaker 1", StartTime: "00:00", EndTime: "00:02", Content: " Hello. "},
		// Crosstalk overlaps the previous segment; a zero-length caption is legal.
		{Speaker: "Speaker 2", StartTime: "00:01", EndTime: "00:01", Content: "Hi."},
	}
	got, err := renderSRT(segments, true)
	want := "1\n00:00:00,000 --> 00:00:02,000\nSpeaker 1: Hello.\n\n" +
		"2\n00:00:01,000 --> 00:00:01,000\nSpeaker 2: Hi.\n\n"
	if err != nil || got != want {
		t.Errorf("renderSRT = %q, %v; want %q", got, err, want)
	}

	got, err = renderSRT(segments[:1], false)
	if want := "1\n00:00:00,000 --> 00:00:02,000\nHello.\n\n"; err != nil || got != want {
		t.Errorf("renderSRT without speakers = %q, %v; want %q", got, err, want)
	}

	for wantErr, bad := range map[string][]transcriptSegment{
		"bad start_time":                {{StartTime: "soon", EndTime: "00:01", Content: "x"}},
		"bad end_time":                  {{StartTime: "00:01", EndTime: "00:99", Content: "x"}},
		"is before start_time":          {{StartTime: "00:05", EndTime: "00:04", Content: "x"}},
		"before the previous segment's": {{StartTime: "00:05", EndTime: "00:06", Content: "x"}, {StartTime: "00:04", EndTime: "00:07", Content: "y"}},
	} {
		if got, err := renderSRT(bad, false); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("renderSRT(%+v) = %q, %v; want an error containing %q", bad, got, err, wantErr)
		}
	}
}

func TestNormalizeFileID(t *testing.T) {
	maxID := strings.Repeat("a", 40)
	for in, wantID := range map[string]string{"files/abc-1": "abc-1", "a": "a", "  files/abc  ": "abc", maxID: maxID} {
		name, id, ok := normalizeFileID(in)
		if !ok || id != wantID || name != "files/"+wantID {
			t.Errorf("normalizeFileID(%q) = %q, %q, %t; want files/%s", in, name, id, ok, wantID)
		}
	}
	for _, in := range []string{"", "files/", "files/a/b", "a/../b", "files/a b", "files/a?b",
		"files/ABC", "abc_2", "abc.x", "-abc", "abc-", maxID + "a"} {
		if name, id, ok := normalizeFileID(in); ok {
			t.Errorf("normalizeFileID(%q) = %q, %q; want rejection", in, name, id)
		}
	}
}

func TestDetectMIME(t *testing.T) {
	tests := []struct{ path, override, want string }{
		{"clip.MP3", "", "audio/mp3"},
		{"clip.mov", "", "video/mov"},
		{"doc.pdf", "", "application/pdf"},
		{"clip.mp3", " audio/wav ", "audio/wav"},
		{"clip.mp3", "Audio/WAV", "audio/wav"},
		{"scan.TIFF", "", "image/tiff"},
		{"clip.mkv", "", "video/x-matroska"},
		{"noext", "", ""},
	}
	for _, tt := range tests {
		if got := detectMIME(tt.path, tt.override); got != tt.want {
			t.Errorf("detectMIME(%q, %q) = %q, want %q", tt.path, tt.override, got, tt.want)
		}
	}
}

func TestContentClassOf(t *testing.T) {
	tests := []struct {
		mime         string
		wantClass    contentClass
		wantSendable bool
	}{
		{"image/png", contentImage, true},
		{"image/svg+xml", contentImage, false},
		{"audio/mp3", contentAudio, true},
		{"audio/amr", contentAudio, false},
		{"video/mov", contentVideo, true},
		{"video/quicktime", contentVideo, false},
		{"video/x-matroska", contentVideo, false},
		{"application/pdf", contentDocument, true},
		// CSV is a document by enum even though it is textual.
		{"text/csv", contentDocument, true},
		{"text/plain", contentText, true},
		{"text/x-go", contentText, true},
		{"application/json", contentText, true},
		{"application/rtf", contentDocument, false},
		{"application/octet-stream", contentDocument, false},
		{"", contentDocument, false},
	}
	for _, tt := range tests {
		if class, sendable := contentClassOf(tt.mime); class != tt.wantClass || sendable != tt.wantSendable {
			t.Errorf("contentClassOf(%q) = %d, %t; want %d, %t", tt.mime, class, sendable, tt.wantClass, tt.wantSendable)
		}
	}
}

// TestMIMETableIsSendable pins every curated extension to a MIME type the
// Interactions API accepts, so detection never yields a request it rejects.
func TestMIMETableIsSendable(t *testing.T) {
	for ext, mimeType := range mimeByExtension {
		if _, sendable := contentClassOf(mimeType); !sendable {
			t.Errorf("mimeByExtension[%q] = %q is outside the interactions mime_type enums", ext, mimeType)
		}
		if ext != strings.ToLower(ext) || !strings.HasPrefix(ext, ".") {
			t.Errorf("mimeByExtension key %q must be a lower-case extension", ext)
		}
	}
	// An upload-only entry that became sendable belongs in mimeByExtension.
	for ext, mimeType := range uploadOnlyMIME {
		if _, sendable := contentClassOf(mimeType); sendable {
			t.Errorf("uploadOnlyMIME[%q] = %q is sendable; move it to mimeByExtension", ext, mimeType)
		}
		if _, dup := mimeByExtension[ext]; dup {
			t.Errorf("extension %q is in both MIME tables", ext)
		}
	}
}

func TestIsYouTubeURL(t *testing.T) {
	for _, in := range []string{"https://youtu.be/x", "https://www.youtube.com/watch?v=x", "http://m.youtube.com/watch?v=x", "https://music.youtube.com/watch?v=x"} {
		if !isYouTubeURL(in) {
			t.Errorf("isYouTubeURL(%q) = false", in)
		}
	}
	for _, in := range []string{"youtu.be/x", "https://youtube.com.evil.example/x", "https://example.com/youtu.be", "ftp://youtu.be/x"} {
		if isYouTubeURL(in) {
			t.Errorf("isYouTubeURL(%q) = true", in)
		}
	}
}

func TestMediaContentBlock(t *testing.T) {
	tests := []struct {
		mime     string
		wantType interactions.ContentType
	}{
		{"image/png", interactions.ContentTypeImage},
		{"audio/mp3", interactions.ContentTypeAudio},
		{"video/mp4", interactions.ContentTypeVideo},
		{"application/pdf", interactions.ContentTypeDocument},
		{"", interactions.ContentTypeDocument},
	}
	for _, tt := range tests {
		for _, inline := range []bool{true, false} {
			block := mediaContentBlock(tt.mime, "payload", inline)
			if block.Type != tt.wantType {
				t.Errorf("mediaContentBlock(%q).Type = %q, want %q", tt.mime, block.Type, tt.wantType)
			}
			var data, uri *string
			switch {
			case block.ImageContent != nil:
				data, uri = block.ImageContent.Data, block.ImageContent.URI
			case block.AudioContent != nil:
				data, uri = block.AudioContent.Data, block.AudioContent.URI
			case block.VideoContent != nil:
				data, uri = block.VideoContent.Data, block.VideoContent.URI
			case block.DocumentContent != nil:
				data, uri = block.DocumentContent.Data, block.DocumentContent.URI
			}
			set, unset := data, uri
			if !inline {
				set, unset = uri, data
			}
			if set == nil || *set != "payload" || unset != nil {
				t.Errorf("mediaContentBlock(%q, inline=%t): data=%v uri=%v", tt.mime, inline, data, uri)
			}
		}
	}
}

func TestTranscriptSchemaRequiredKeys(t *testing.T) {
	tests := []struct {
		speakers, timestamps bool
		want                 []string
	}{
		{true, true, []string{"content", "speaker", "start_time", "end_time"}},
		{false, true, []string{"content", "start_time", "end_time"}},
		{true, false, []string{"content", "speaker"}},
		{false, false, []string{"content"}},
	}
	for _, tt := range tests {
		schema := transcriptSchema(tt.speakers, tt.timestamps)
		items := schema["properties"].(map[string]any)["segments"].(map[string]any)["items"].(map[string]any)
		if got := items["required"].([]string); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("transcriptSchema(%t, %t) required = %v, want %v", tt.speakers, tt.timestamps, got, tt.want)
		}
		props := items["properties"].(map[string]any)
		if len(props) != len(tt.want) {
			t.Errorf("transcriptSchema(%t, %t) properties = %v, want exactly %v", tt.speakers, tt.timestamps, props, tt.want)
		}
	}
}

func TestParseAudioMIME(t *testing.T) {
	tests := []struct {
		mime string
		want audioFormat
	}{
		{"audio/L16;codec=pcm;rate=24000", audioFormat{baseMIME: "audio/l16", sampleRate: 24000, channels: 1, isPCM: true}},
		{" audio/pcm ; Rate = 16000 ; channels=2 ", audioFormat{baseMIME: "audio/pcm", sampleRate: 16000, channels: 2, isPCM: true}},
		{"audio/l16", audioFormat{baseMIME: "audio/l16", channels: 1, isPCM: true}},
		// Unparseable or non-positive parameters are ignored, not trusted.
		{"audio/l16;rate=fast;channels=0;codec", audioFormat{baseMIME: "audio/l16", channels: 1, isPCM: true}},
		// Compressed audio is never PCM and gets no channel default.
		{"audio/mp3", audioFormat{baseMIME: "audio/mp3"}},
		{"audio/ogg;rate=48000", audioFormat{baseMIME: "audio/ogg", sampleRate: 48000}},
		{"", audioFormat{}},
	}
	for _, tt := range tests {
		if got := parseAudioMIME(tt.mime); got != tt.want {
			t.Errorf("parseAudioMIME(%q) = %+v, want %+v", tt.mime, got, tt.want)
		}
	}
}

func TestWavHeader(t *testing.T) {
	tests := []struct {
		pcmLen, sampleRate, channels int
		wantByteRate                 uint32
		wantBlockAlign               uint16
	}{
		{1000, 24000, 1, 48000, 2},
		{4, 48000, 2, 192000, 4},
		{0, 16000, 1, 32000, 2},
	}
	for _, tt := range tests {
		h := wavHeader(tt.pcmLen, tt.sampleRate, tt.channels)
		if len(h) != 44 {
			t.Fatalf("wavHeader length = %d, want 44", len(h))
		}
		for offset, want := range map[int]string{0: "RIFF", 8: "WAVE", 12: "fmt ", 36: "data"} {
			if got := string(h[offset : offset+4]); got != want {
				t.Errorf("wavHeader[%d:] = %q, want %q", offset, got, want)
			}
		}
		u16 := func(o int) uint16 { return binary.LittleEndian.Uint16(h[o:]) }
		u32 := func(o int) uint32 { return binary.LittleEndian.Uint32(h[o:]) }
		if u32(4) != uint32(36+tt.pcmLen) || u32(40) != uint32(tt.pcmLen) {
			t.Errorf("RIFF/data sizes = %d/%d, want %d/%d", u32(4), u32(40), 36+tt.pcmLen, tt.pcmLen)
		}
		if u32(16) != 16 || u16(20) != 1 || u16(34) != 16 {
			t.Errorf("fmt size/format/bits = %d/%d/%d, want 16/1 (PCM)/16", u32(16), u16(20), u16(34))
		}
		if u16(22) != uint16(tt.channels) || u32(24) != uint32(tt.sampleRate) {
			t.Errorf("channels/rate = %d/%d, want %d/%d", u16(22), u32(24), tt.channels, tt.sampleRate)
		}
		if u32(28) != tt.wantByteRate || u16(32) != tt.wantBlockAlign {
			t.Errorf("byteRate/blockAlign = %d/%d, want %d/%d", u32(28), u16(32), tt.wantByteRate, tt.wantBlockAlign)
		}
	}
}

func TestExtensionForAudioMIME(t *testing.T) {
	for mime, want := range map[string]string{
		"audio/mp3": ".mp3", "audio/mpeg": ".mp3", "audio/aac": ".aac", "audio/ogg": ".ogg",
		"audio/vorbis": ".ogg", "audio/flac": ".flac", "audio/opus": ".opus", "audio/m4a": ".m4a",
		"audio/mp4": ".m4a", "AUDIO/MP3; rate=44100": ".mp3",
		"audio/ogg_opus": ".ogg",
		// Raw PCM is wrapped as WAV; a WAV response already is one.
		"audio/l16;rate=24000": ".wav", "audio/wav": ".wav",
	} {
		if got, ok := extensionForAudioMIME(mime); !ok || got != want {
			t.Errorf("extensionForAudioMIME(%q) = %q, %t; want %q", mime, got, ok, want)
		}
	}
	// Headerless companded audio and unknown types have no playable form.
	for _, mime := range []string{"audio/alaw", "audio/mulaw", "audio/l8", "audio/unknown", ""} {
		if got, ok := extensionForAudioMIME(mime); ok {
			t.Errorf("extensionForAudioMIME(%q) = %q, want it rejected", mime, got)
		}
	}
}

// ttsCommand is a standalone tts command with the given flags parsed.
func ttsCommand(t *testing.T, flags ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "tts"}
	attachTTS(cmd)
	if err := cmd.ParseFlags(flags); err != nil {
		t.Fatalf("parsing %v: %v", flags, err)
	}
	return cmd
}

func TestArtifactPath(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	for out, want := range map[string]string{
		"speech.wav":                     filepath.Join(dir, "speech.wav"),
		"speech.WAV":                     filepath.Join(dir, "speech.WAV"),
		"speech":                         filepath.Join(dir, "speech.wav"),
		"speech.mp3":                     filepath.Join(dir, "speech.wav"),
		"v1.2/speech.mp3":                filepath.Join(dir, "v1.2", "speech.wav"),
		filepath.Join(dir, "abs", "a.b"): filepath.Join(dir, "abs", "a.wav"),
	} {
		got, err := artifactPath(ttsCommand(t, "--out", out), "out", "gemini-tts", ".wav")
		if err != nil || got != want {
			t.Errorf("artifactPath(--out %q) = %q, %v; want %q", out, got, err, want)
		}
	}

	defaultName := regexp.MustCompile(`^gemini-tts-\d+-[0-9a-f]{6}\.wav$`)
	// A directory — by trailing separator or because it exists — receives the
	// default-named file instead of becoming "<dir>.wav" or "<dir>/.wav".
	if err := os.Mkdir(filepath.Join(dir, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	for out, wantDir := range map[string]string{
		"existing":  filepath.Join(dir, "existing"),
		"existing/": filepath.Join(dir, "existing"),
		"fresh/":    filepath.Join(dir, "fresh"),
	} {
		got, err := artifactPath(ttsCommand(t, "--out", out), "out", "gemini-tts", ".wav")
		if err != nil || filepath.Dir(got) != wantDir || !defaultName.MatchString(filepath.Base(got)) {
			t.Errorf("artifactPath(--out %q) = %q, %v; want %s/gemini-tts-<ms>-<hex>.wav", out, got, err, wantDir)
		}
	}

	seen := map[string]bool{}
	for _, flags := range [][]string{nil, {"--out", "  "}, nil} {
		got, err := artifactPath(ttsCommand(t, flags...), "out", "gemini-tts", ".wav")
		if err != nil || filepath.Dir(got) != dir || !defaultName.MatchString(filepath.Base(got)) {
			t.Errorf("artifactPath(%v) = %q, %v; want %s/gemini-tts-<ms>-<hex>.wav", flags, got, err, dir)
		}
		if seen[got] {
			t.Errorf("artifactPath default %q repeated", got)
		}
		seen[got] = true
	}
}

func TestParseTranscript(t *testing.T) {
	full := `{"segments":[{"speaker":"Speaker 1","start_time":"00:00","end_time":"00:02","content":"Hello."}]}`
	want := []transcriptSegment{{Speaker: "Speaker 1", StartTime: "00:00", EndTime: "00:02", Content: "Hello."}}
	for name, raw := range map[string]string{
		"bare":            full,
		"padded":          "\n  " + full + "\n",
		"json fence":      "```json\n" + full + "\n```",
		"anonymous fence": "```\n" + full + "\n```",
	} {
		got, err := parseTranscript(raw, true, true)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseTranscript(%s) = %+v, %v; want %+v", name, got, err, want)
		}
	}

	// A missing speaker is labelled rather than rejected, and only when asked for.
	noSpeaker := `{"segments":[{"content":"Hi.","start_time":"0","end_time":"1"}]}`
	if got, err := parseTranscript(noSpeaker, true, true); err != nil || got[0].Speaker != "Speaker" {
		t.Errorf("parseTranscript(speakers on) = %+v, %v; want the placeholder speaker", got, err)
	}
	if got, err := parseTranscript(noSpeaker, false, true); err != nil || got[0].Speaker != "" {
		t.Errorf("parseTranscript(speakers off) = %+v, %v; want no speaker", got, err)
	}
	// Timestamps are only required when requested.
	if _, err := parseTranscript(`{"segments":[{"content":"Hi."}]}`, false, false); err != nil {
		t.Errorf("parseTranscript(timestamps off) rejected a bare segment: %v", err)
	}

	for name, tt := range map[string]struct{ raw, wantErr string }{
		"not json":       {"Speaker 1: hello", "not valid JSON"},
		"no segments":    {`{"segments":[]}`, "no segments"},
		"wrong shape":    {`{"transcript":"hello"}`, "no segments"},
		"blank content":  {`{"segments":[{"content":"  ","start_time":"0","end_time":"1"}]}`, "segment 1 has no content"},
		"missing end":    {`{"segments":[{"content":"a","start_time":"0","end_time":"1"},{"content":"b","start_time":"1"}]}`, "segment 2 is missing start_time/end_time"},
		"unclosed fence": {"```json\n" + full[:len(full)-1], "not valid JSON"},
	} {
		if got, err := parseTranscript(tt.raw, true, true); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("parseTranscript(%s) = %+v, %v; want an error containing %q", name, got, err, tt.wantErr)
		}
	}
}

func TestBuildSpeechConfig(t *testing.T) {
	speech := func(speaker, voice, language string) interactions.SpeechConfig {
		c := interactions.SpeechConfig{Voice: stringPtr(voice)}
		if speaker != "" {
			c.Speaker = stringPtr(speaker)
		}
		if language != "" {
			c.Language = stringPtr(language)
		}
		return c
	}
	single := []struct {
		flags []string
		want  interactions.SpeechConfig
	}{
		{nil, speech("", defaultTTSVoice, "")},
		{[]string{"--voice", " Puck ", "--language", " en-US "}, speech("", "Puck", "en-US")},
		{[]string{"--voice", " "}, speech("", defaultTTSVoice, "")},
		{[]string{"--voice", "Puck", "--multi-speaker", "  "}, speech("", "Puck", "")},
	}
	for _, tt := range single {
		got, label, err := buildSpeechConfig(ttsCommand(t, tt.flags...))
		if err != nil || label != *tt.want.Voice || got.SpeakerConfig != nil || !reflect.DeepEqual(got.ArrayOfSpeechConfig, []interactions.SpeechConfig{tt.want}) {
			t.Errorf("buildSpeechConfig(%v) = %+v, %v; want the single voice %+v", tt.flags, got, err, tt.want)
		}
	}

	got, label, err := buildSpeechConfig(ttsCommand(t, "--multi-speaker", " Alice = Kore ,, Bob=Puck, ", "--language", "en-GB"))
	wantSpeakers := []interactions.SpeechConfig{speech("Alice", "Kore", "en-GB"), speech("Bob", "Puck", "en-GB")}
	if err != nil || label != "multi-speaker Alice = Kore ,, Bob=Puck," || got.ArrayOfSpeechConfig != nil || got.SpeakerConfig == nil || !reflect.DeepEqual(got.SpeakerConfig.Speakers, wantSpeakers) {
		t.Errorf("buildSpeechConfig(multi-speaker) = %+v, %v; want speakers %+v", got, err, wantSpeakers)
	}

	for wantErr, flags := range map[string][]string{
		"mutually exclusive":                        {"--voice", "Puck", "--multi-speaker", "A=Kore"},
		`invalid --multi-speaker entry "A"`:         {"--multi-speaker", "A"},
		`entry "A="`:                                {"--multi-speaker", "A="},
		`entry "=Kore"`:                             {"--multi-speaker", "B=Puck,=Kore"},
		"exactly two Speaker=Voice entries (got 0)": {"--multi-speaker", " , ,"},
		"exactly two Speaker=Voice entries (got 1)": {"--multi-speaker", "A=Kore"},
		"exactly two Speaker=Voice entries (got 3)": {"--multi-speaker", "A=Kore,B=Puck,C=Zephyr"},
		`speaker "A" more than once`:                {"--multi-speaker", "A=Kore,A=Puck"},
		`speaker "A B" more than once`:              {"--multi-speaker", "A  B=Kore, A B =Puck"},
	} {
		if got, _, err := buildSpeechConfig(ttsCommand(t, flags...)); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("buildSpeechConfig(%v) = %+v, %v; want an error containing %q", flags, got, err, wantErr)
		}
	}
}

func TestSpeakerTurns(t *testing.T) {
	turn := func(speaker, text string) speakerTurn { return speakerTurn{speaker: speaker, text: text} }
	alice, bob := turn("Alice", "hi."), turn("Bob", "yo.")
	valid := []struct {
		text     string
		speakers []string
		want     []speakerTurn
	}{
		{"Alice: hi. Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{alice, bob}},
		{"  Alice: hi.\nBob:yo.\n\nAlice : bye.\n", []string{"Alice", "Bob"}, []speakerTurn{alice, bob, turn("Alice", "bye.")}},
		{"Alice: hi.\r\nBob: yo.\r\n", []string{"Alice", "Bob"}, []speakerTurn{alice, bob}},
		{"Alice: hi\nBob: yo", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "hi"), turn("Bob", "yo")}},
		// Every rune strings.Fields splits on separates labels and name words.
		{"Alice: hi.\u00a0Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{alice, bob}},
		{"Alice: hi.\u3000Bob\u2028: yo.", []string{"Alice", "Bob"}, []speakerTurn{alice, bob}},
		{"Dr\u00a0 Who: hi.", []string{"Dr Who"}, []speakerTurn{turn("Dr Who", "hi.")}},
		// A label may follow punctuation or a line break, but not a word on the same line.
		{"Alice: \"hi.\"Bob: \"yo.\"", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "\"hi.\""), turn("Bob", "\"yo.\"")}},
		{"Ann: hi. MaryAnn: x. 2Ann: y. Ann: yo.", []string{"Ann", "Bo"}, []speakerTurn{turn("Ann", "hi. MaryAnn: x. 2Ann: y."), turn("Ann", "yo.")}},
		{"Alice: I told Bob: wait here.\nBob: yo.", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "I told Bob: wait here."), bob}},
		// Line-initial bullets before speaker labels are stripped cleanly on every turn.
		{"- Alice: hi.\n- Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{alice, bob}},
		{"* Alice: hi.\n• Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{alice, bob}},
		{"> Alice: hi.\n  >> Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{alice, bob}},
		{"1. Alice: hi.\n2) Bob: yo.\n10. Alice: bye.", []string{"Alice", "Bob"}, []speakerTurn{alice, bob, turn("Alice", "bye.")}},
		// A quote opening the line goes with the label; a closing one stays in the turn.
		{"\"Alice: hi.\"\n\"Bob: yo.\"", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "hi.\""), turn("Bob", "yo.\"")}},
		{"\u201cAlice: hi.\u201d Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "hi.\u201d"), bob}},
		// Punctuation or digits after words on the same line stay in the previous turn.
		{"Alice: hi. - Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "hi. -"), bob}},
		{"Alice: I have 2. Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "I have 2."), bob}},
		{"Alice: 42. Bob: That is correct.", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "42."), turn("Bob", "That is correct.")}},
		// A combining mark is part of the word: neither "\u0936\u094d\u0930\u0940\u0930\u093e\u092e:" nor "\u0930\u093e\u092e\u0940:" holds a label for "\u0930\u093e\u092e".
		{"Alice: \u0936\u094d\u0930\u0940\u0930\u093e\u092e: \u0930\u093e\u092e\u0940: hi. \u0930\u093e\u092e: yo.", []string{"Alice", "\u0930\u093e\u092e"}, []speakerTurn{turn("Alice", "\u0936\u094d\u0930\u0940\u0930\u093e\u092e: \u0930\u093e\u092e\u0940: hi."), turn("\u0930\u093e\u092e", "yo.")}},
		// Marker scanning stops at the previous label, so names made of marker
		// runes stay labels; a punctuation-only turn after a label is kept.
		{"\U0001F600: \U0001F916: hi.", []string{"\U0001F600", "\U0001F916"}, []speakerTurn{turn("\U0001F916", "hi.")}},
		{"\U0001F600: hi.\n\U0001F916: yo.", []string{"\U0001F600", "\U0001F916"}, []speakerTurn{turn("\U0001F600", "hi."), turn("\U0001F916", "yo.")}},
		{"Alice: - Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "-"), bob}},
		{"Alice: ... Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "..."), bob}},
		{"Alice: \u2014 Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{turn("Alice", "\u2014"), bob}},
		// "Annual:" is not a label for "Ann"; an undeclared "Carol:" stays in the turn.
		{"Ann: Annual: report. Carol: yes. Bo: ok.", []string{"Ann", "Bo"}, []speakerTurn{turn("Ann", "Annual: report. Carol: yes."), turn("Bo", "ok.")}},
		// The longer name wins over a declared prefix of it.
		{"Alice: hi. Al: yo.", []string{"Al", "Alice"}, []speakerTurn{alice, turn("Al", "yo.")}},
		{"Dr  Who: hi.\nA.B: yo. AxB: no.", []string{"Dr Who", "A.B"}, []speakerTurn{turn("Dr Who", "hi."), turn("A.B", "yo. AxB: no.")}},
		// A label with nothing to say is dropped, whether or not space follows.
		{"Alice: Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{bob}},
		{"Alice:Bob: yo.", []string{"Alice", "Bob"}, []speakerTurn{bob}},
	}
	for _, tt := range valid {
		if got, err := speakerTurns(tt.text, tt.speakers); err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("speakerTurns(%q, %q) = %+v, %v; want %+v", tt.text, tt.speakers, got, err, tt.want)
		}
	}

	for text, wantErr := range map[string]string{
		"hi. Bob: yo.":                      `text before the first speaker label: "hi."`,
		"- hi. Bob: yo.":                    `text before the first speaker label: "- hi."`,
		"(intro) Bob: yo.":                  `text before the first speaker label: "(intro)"`,
		"1.5 Bob: yo.":                      `text before the first speaker label: "1.5"`,
		strings.Repeat("é", 70) + " Bob: x": `label: "` + strings.Repeat("é", 60) + `…"`,
		"alice: hi. bob: yo.":               "the text has no speaker label",
		"Alice:  \n Bob: \n":                "every speaker turn is empty",
	} {
		if got, err := speakerTurns(text, []string{"Alice", "Bob"}); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("speakerTurns(%q) = %+v, %v; want an error containing %q", text, got, err, wantErr)
		}
	}
}

func TestTTSInput(t *testing.T) {
	speech := func(flags ...string) *interactions.SpeechConfigUnion {
		t.Helper()
		s, _, err := buildSpeechConfig(ttsCommand(t, flags...))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	single, multi := speech(), speech("--multi-speaker", "Alice=Kore,Bob=Puck")
	const script = "Alice: hi. Bob: yo."
	plain := []interactions.Content{textContentBlock(script)}
	annotated := func(speaker, text string) interactions.Content {
		return interactions.CreateContentText(interactions.TextContent{
			Text: text,
			Annotations: []interactions.Annotation{interactions.CreateAnnotationSpeechMetadata(
				interactions.SpeechAnnotation{Speaker: stringPtr(speaker)})},
		})
	}
	turns := []interactions.Content{annotated("Alice", "hi."), annotated("Bob", "yo.")}
	for _, tt := range []struct {
		model  string
		speech *interactions.SpeechConfigUnion
		want   []interactions.Content
	}{
		{defaultTTSModel, single, plain},
		{defaultTTSModel, multi, turns},
		{"gemini-3.8-flash-lite-tts", multi, turns},
		{"gemini-3.1-flash-tts-preview", multi, plain},
		{"gemini-2.5-flash-preview-tts", multi, plain},
		{"gemini-2.5-pro-preview-tts", multi, plain},
		{"gemini-2.5-flash-tts", multi, plain},
		{"gemini-2.5-pro-tts", multi, plain},
	} {
		if got, absent, err := ttsInput(tt.model, script, tt.speech); err != nil || absent != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ttsInput(%s, multi=%t) = %+v, %q, %v; want %+v", tt.model, tt.speech.SpeakerConfig != nil, got, absent, err, tt.want)
		}
	}

	// Absent speakers follow the parsed turns on current models and a word
	// match on legacy ones.
	for _, tt := range []struct {
		model, text string
		speech      *interactions.SpeechConfigUnion
		want        []string
	}{
		{defaultTTSModel, "Dr  Who: hi. Bob: yo.", speech("--multi-speaker", "Dr Who=Kore,Bob=Puck"), nil},
		{defaultTTSModel, "Alice: hi, Bob.", multi, []string{"Bob"}},
		{"gemini-3.1-flash-tts-preview", "Alice: hi, Bob.", multi, nil},
		{"gemini-3.1-flash-tts-preview", "Alice: hi.", multi, []string{"Bob"}},
		{"gemini-3.1-flash-tts-preview", "Alice: \u0936\u094d\u0930\u0940\u0930\u093e\u092e \u0930\u093e\u092e\u0940.", speech("--multi-speaker", "Alice=Kore,\u0930\u093e\u092e=Puck"), []string{"\u0930\u093e\u092e"}},
	} {
		if _, absent, err := ttsInput(tt.model, tt.text, tt.speech); err != nil || !slices.Equal(absent, tt.want) {
			t.Errorf("ttsInput(%s, %q) absent = %q, %v; want %q", tt.model, tt.text, absent, err, tt.want)
		}
	}
}

func TestDecodeText(t *testing.T) {
	for in, want := range map[string]string{
		"\uFEFFAlice: hi.\r\n": "Alice: hi.",
		"  hi \n":              "hi",
		"hi\uFEFF":             "hi\uFEFF",
	} {
		if got := decodeText([]byte(in)); got != want {
			t.Errorf("decodeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeIdentifier(t *testing.T) {
	newWrappedCmd := func(t *testing.T, flagName, shorthand, desc string, normalize func(string) (string, error), gotVal *string, called *bool) *cobra.Command {
		t.Helper()
		cmd := &cobra.Command{
			Use:           "get [" + flagName + "]",
			Args:          flagutil.PositionalFlagArgs,
			SilenceUsage:  true,
			SilenceErrors: true,
			RunE: func(c *cobra.Command, args []string) error {
				if err := flagutil.ResolvePositionalFlag(c, args); err != nil {
					return err
				}
				*called = true
				*gotVal, _ = c.Flags().GetString(flagName)
				return nil
			},
		}
		cmd.Flags().StringP(flagName, shorthand, "", desc+" [required]")
		if err := flagutil.DeclarePositionalFlag(cmd, flagName, desc+" (or pass it as the ["+flagName+"] argument)", true); err != nil {
			t.Fatalf("DeclarePositionalFlag: %v", err)
		}
		if err := normalizeIdentifier(cmd, flagName, normalize); err != nil {
			t.Fatalf("normalizeIdentifier: %v", err)
		}
		return cmd
	}

	tests := []struct {
		name      string
		flagName  string
		shorthand string
		desc      string
		normalize func(string) (string, error)
		args      []string
		wantVal   string
		wantErr   string
	}{
		{
			name:      "files get positional prefixed",
			flagName:  "file",
			shorthand: "f",
			desc:      "File to get, as files/<id> or a bare id",
			normalize: normalizeFilePositional,
			args:      []string{"files/abc"},
			wantVal:   "abc",
		},
		{
			name:      "files get positional bare",
			flagName:  "file",
			shorthand: "f",
			desc:      "File to get, as files/<id> or a bare id",
			normalize: normalizeFilePositional,
			args:      []string{"abc"},
			wantVal:   "abc",
		},
		{
			name:      "files get flag prefixed",
			flagName:  "file",
			shorthand: "f",
			desc:      "File to get, as files/<id> or a bare id",
			normalize: normalizeFilePositional,
			args:      []string{"--file", "files/abc"},
			wantVal:   "abc",
		},
		{
			name:      "files get invalid positional",
			flagName:  "file",
			shorthand: "f",
			desc:      "File to get, as files/<id> or a bare id",
			normalize: normalizeFilePositional,
			args:      []string{"../x"},
			wantErr:   `invalid file id "../x"; expected files/<id> or a bare id`,
		},
		{
			name:      "files get invalid flag",
			flagName:  "file",
			shorthand: "f",
			desc:      "File to get, as files/<id> or a bare id",
			normalize: normalizeFilePositional,
			args:      []string{"--file", "../x"},
			wantErr:   `invalid file id "../x"; expected files/<id> or a bare id`,
		},
		{
			name:      "files get missing id",
			flagName:  "file",
			shorthand: "f",
			desc:      "File to get, as files/<id> or a bare id",
			normalize: normalizeFilePositional,
			args:      nil,
			wantErr:   "missing required flag: --file (or pass it as the [file] argument)",
		},
		{
			name:      "files get id passed twice",
			flagName:  "file",
			shorthand: "f",
			desc:      "File to get, as files/<id> or a bare id",
			normalize: normalizeFilePositional,
			args:      []string{"abc", "--file", "def"},
			wantErr:   "pass file once: as the [file] argument or via --file, not both",
		},
		{
			name:      "files get flag-like positional after --",
			flagName:  "file",
			shorthand: "f",
			desc:      "File to get, as files/<id> or a bare id",
			normalize: normalizeFilePositional,
			args:      []string{"--", "--dry-run"},
			wantErr:   `argument "--dry-run" looks like a flag; pass a value starting with "-" as --file=--dry-run`,
		},
		{
			name:      "models get positional prefixed",
			flagName:  "model",
			shorthand: "m",
			desc:      "Model id, e.g. gemini-flash-latest",
			normalize: normalizeModelPositional,
			args:      []string{"models/gemini-flash-latest"},
			wantVal:   "gemini-flash-latest",
		},
		{
			name:      "models get flag bare",
			flagName:  "model",
			shorthand: "m",
			desc:      "Model id, e.g. gemini-flash-latest",
			normalize: normalizeModelPositional,
			args:      []string{"--model", "gemini-flash-latest"},
			wantVal:   "gemini-flash-latest",
		},
		{
			name:      "models get invalid flag",
			flagName:  "model",
			shorthand: "m",
			desc:      "Model id, e.g. gemini-flash-latest",
			normalize: normalizeModelPositional,
			args:      []string{"--model", "../files/x"},
			wantErr:   `invalid model id "../files/x"`,
		},
		{
			name:      "models get missing id",
			flagName:  "model",
			shorthand: "m",
			desc:      "Model id, e.g. gemini-flash-latest",
			normalize: normalizeModelPositional,
			args:      nil,
			wantErr:   "missing required flag: --model (or pass it as the [model] argument)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotVal string
			var called bool
			cmd := newWrappedCmd(t, tt.flagName, tt.shorthand, tt.desc, tt.normalize, &gotVal, &called)
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Execute(%v) error = %v, want substring %q", tt.args, err, tt.wantErr)
				}
				if called {
					t.Errorf("Execute(%v) invoked original RunE on validation failure", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute(%v) unexpected error: %v", tt.args, err)
			}
			if !called || gotVal != tt.wantVal {
				t.Errorf("Execute(%v) called=%t, gotVal=%q; want true, %q", tt.args, called, gotVal, tt.wantVal)
			}
		})
	}
}

func TestNormalizeEnvironmentFilesList(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantEnv  string
		wantPath string
		wantErr  string
	}{
		{
			name:     "bare environment and relative path",
			args:     []string{"--environment", "env_abc123", "--path", "src"},
			wantEnv:  "env_abc123",
			wantPath: "src",
		},
		{
			name:     "prefixed environment and leading slash path",
			args:     []string{"--environment", "environments/env_abc123", "--path", "/var/mail"},
			wantEnv:  "env_abc123",
			wantPath: "var/mail",
		},
		{
			name:     "slash-only path maps to root sentinel",
			args:     []string{"--environment", "env_abc123", "--path", "/"},
			wantEnv:  "env_abc123",
			wantPath: environmentRootSentinel,
		},
		{
			name:     "dot path maps to root sentinel",
			args:     []string{"--environment", "env_abc123", "--path", "."},
			wantEnv:  "env_abc123",
			wantPath: environmentRootSentinel,
		},
		{
			name:     "dot-slash relative path stripped",
			args:     []string{"--environment", "env_abc123", "--path", "./src"},
			wantEnv:  "env_abc123",
			wantPath: "src",
		},
		{
			name:    "invalid environment with slash rejected",
			args:    []string{"--environment", "environments/a/b", "--path", "src"},
			wantErr: `invalid environment id "environments/a/b"`,
		},
		{
			name:     "missing environment deferred to generated validation",
			args:     []string{"--path", "src"},
			wantPath: "src",
		},
		{
			name:     "missing path maps to root sentinel",
			args:     []string{"--environment", "env_abc123"},
			wantEnv:  "env_abc123",
			wantPath: environmentRootSentinel,
		},
		{
			name:     "blank environment deferred to generated validation",
			args:     []string{"--environment", " ", "--path", "src"},
			wantEnv:  " ",
			wantPath: "src",
		},
		{
			name:     "blank path maps to root sentinel",
			args:     []string{"--environment", "env_abc123", "--path", ""},
			wantEnv:  "env_abc123",
			wantPath: environmentRootSentinel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotEnv, gotPath string
			var called bool
			cmd := &cobra.Command{
				Use:  "list",
				Args: cobra.NoArgs,
				RunE: func(c *cobra.Command, _ []string) error {
					called = true
					gotEnv, _ = c.Flags().GetString("environment")
					gotPath, _ = c.Flags().GetString("path")
					return nil
				},
			}
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			flagutil.RegisterFlags(cmd, []flagutil.FlagMeta{
				{FlagName: "environment", Shorthand: "e", FieldPath: "Environment", Kind: flagutil.FlagKindString, Required: true, Description: "[required]"},
				{FlagName: "path", Shorthand: "p", FieldPath: "Path", Kind: flagutil.FlagKindString, Required: true, Description: "[required]"},
			})
			if err := normalizeEnvironmentFilesList(cmd); err != nil {
				t.Fatalf("normalizeEnvironmentFilesList: %v", err)
			}
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Execute(%v) error = %v, want substring %q", tt.args, err, tt.wantErr)
				}
				if called {
					t.Errorf("Execute(%v) invoked original RunE on validation failure", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute(%v) unexpected error: %v", tt.args, err)
			}
			if !called || gotEnv != tt.wantEnv || gotPath != tt.wantPath {
				t.Errorf("Execute(%v) called=%t, gotEnv=%q, gotPath=%q; want true, %q, %q", tt.args, called, gotEnv, gotPath, tt.wantEnv, tt.wantPath)
			}
		})
	}
}

func newTestEnvironmentFilesRoot(serverURL string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	root := &cobra.Command{Use: "gemini-api", SilenceErrors: true, SilenceUsage: true}
	root.PersistentFlags().StringP("output-format", "o", "pretty", "")
	root.PersistentFlags().String("color", "never", "")
	root.PersistentFlags().StringP("jq", "q", "", "")
	root.PersistentFlags().Bool("raw-output", true, "")
	root.PersistentFlags().String("server-url", serverURL, "")
	root.PersistentFlags().StringArrayP("header", "H", nil, "")
	root.PersistentFlags().Bool("include-headers", false, "")
	root.PersistentFlags().String("timeout", "", "")
	root.PersistentFlags().Bool("interactive", false, "")
	root.PersistentFlags().Bool("no-interactive", true, "")
	root.PersistentFlags().Bool("usage", false, "")
	root.PersistentFlags().Bool("dry-run", false, "")
	root.PersistentFlags().BoolP("debug", "d", false, "")
	root.PersistentFlags().Bool("agent-mode", false, "")
	root.PersistentFlags().String("api-key", "test-api-key", "")
	root.PersistentFlags().String("access-token", "", "")
	root.PersistentFlags().String("api-version", "v1beta", "")
	root.PersistentFlags().String("api-revision", "", "")
	root.PersistentFlags().String("user-project", "", "")

	envs := &cobra.Command{Use: "environments"}
	envFiles := &cobra.Command{Use: "files"}
	envFiles.AddCommand(newEnvironmentFilesUploadCmd())
	envFiles.AddCommand(newEnvironmentFilesDownloadCmd())
	envs.AddCommand(envFiles)
	root.AddCommand(envs)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	return root, &stdout, &stderr
}

func TestEnvironmentFilesUploadProtocol(t *testing.T) {
	dir := t.TempDir()
	localFile := filepath.Join(dir, "bundle.tar.gz")
	payload := []byte("synthetic archive bytes for upload test")
	if err := os.WriteFile(localFile, payload, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var (
		startMethod   string
		startPath     string
		startQuery    string
		startHeaders  http.Header
		chunkMethod   string
		chunkHeaders  http.Header
		uploadedBytes []byte
	)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/environments/env_abc123/files/app/bundle.tar.gz":
			startMethod = r.Method
			startPath = r.URL.Path
			startQuery = r.URL.RawQuery
			startHeaders = r.Header.Clone()
			w.Header().Set("X-Goog-Upload-Url", srv.URL+"/upload-session?upload_id=session_xyz")
			w.Header().Set("X-Goog-Upload-Status", "active")
			w.WriteHeader(http.StatusOK)
		case "/upload-session":
			chunkMethod = r.Method
			chunkHeaders = r.Header.Clone()
			body, _ := io.ReadAll(r.Body)
			uploadedBytes = append(uploadedBytes, body...)
			w.Header().Set("X-Goog-Upload-Status", "final")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"files":[{"name":"environments/env_abc123/files/app/bundle.tar.gz","path":"app/bundle.tar.gz","size_bytes":"39","mime_type":"application/gzip"}]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	root, stdout, _ := newTestEnvironmentFilesRoot(srv.URL)
	root.SetArgs([]string{
		"environments", "files", "upload",
		"environments/env_abc123",
		localFile,
		"--path", "/app/bundle.tar.gz",
		"--overwrite",
		"--extract",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute upload failed: %v", err)
	}

	if startMethod != http.MethodPut {
		t.Errorf("start method = %q, want PUT", startMethod)
	}
	if startPath != "/upload/v1beta/environments/env_abc123/files/app/bundle.tar.gz" {
		t.Errorf("start path = %q", startPath)
	}
	if startQuery != "extract=true&overwrite=true" {
		t.Errorf("start query = %q, want extract=true&overwrite=true", startQuery)
	}
	if got := startHeaders.Get("X-Goog-Upload-Protocol"); got != "resumable" {
		t.Errorf("X-Goog-Upload-Protocol = %q, want resumable", got)
	}
	if got := startHeaders.Get("X-Goog-Upload-Command"); got != "start" {
		t.Errorf("X-Goog-Upload-Command = %q, want start", got)
	}
	if got := startHeaders.Get("X-Goog-Upload-Header-Content-Length"); got != "39" {
		t.Errorf("X-Goog-Upload-Header-Content-Length = %q, want 39", got)
	}
	if got := startHeaders.Get("X-Goog-Upload-Header-Content-Type"); got != "application/gzip" {
		t.Errorf("X-Goog-Upload-Header-Content-Type = %q, want application/gzip", got)
	}
	if chunkMethod != http.MethodPut {
		t.Errorf("chunk method = %q, want PUT", chunkMethod)
	}
	if got := chunkHeaders.Get("X-Goog-Upload-Command"); got != "upload, finalize" {
		t.Errorf("chunk X-Goog-Upload-Command = %q, want 'upload, finalize'", got)
	}
	if got := chunkHeaders.Get("X-Goog-Upload-Offset"); got != "0" {
		t.Errorf("chunk X-Goog-Upload-Offset = %q, want 0", got)
	}
	if !bytes.Equal(uploadedBytes, payload) {
		t.Errorf("uploaded bytes = %q, want %q", uploadedBytes, payload)
	}
	if got := stdout.String(); got != "app/bundle.tar.gz\n" {
		t.Errorf("stdout = %q, want %q", got, "app/bundle.tar.gz\n")
	}
}

func TestEnvironmentFilesUploadDefaultsAndDryRun(t *testing.T) {
	dir := t.TempDir()
	localFile := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(localFile, []byte("hello world\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// --dry-run must preview the start request and never send network bytes.
	root, stdout, _ := newTestEnvironmentFilesRoot(srv.URL)
	root.SetArgs([]string{
		"environments", "files", "upload",
		"env_abc123",
		localFile,
		"--mime-type", "text/custom",
		"--dry-run",
		"-o", "json",
	})
	output.PreparseRenderingFlags(root, []string{"-o", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute dry-run failed: %v", err)
	}
	if requests != 0 {
		t.Errorf("dry-run made %d server requests, want 0", requests)
	}
	var preview map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil {
		t.Fatalf("unmarshal dry-run json %q: %v", stdout.String(), err)
	}
	if preview["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", preview["dry_run"])
	}
	reqObj, _ := preview["request"].(map[string]any)
	if gotURL, _ := reqObj["url"].(string); !strings.HasSuffix(gotURL, "/upload/v1beta/environments/env_abc123/files/hello.txt") {
		t.Errorf("dry-run request url = %q, want suffix /upload/v1beta/environments/env_abc123/files/hello.txt", gotURL)
	}
}

func TestEnvironmentFilesDownload(t *testing.T) {
	binaryContent := []byte{0x00, 0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0xff}
	var gotQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1beta/environments/env_abc123/files/src/logo.png" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(binaryContent)
	}))
	defer srv.Close()

	// 1. Default output path (./<basename>) in working directory.
	workDir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	root, stdout, _ := newTestEnvironmentFilesRoot(srv.URL)
	output.PreparseRenderingFlags(root, nil)
	root.SetArgs([]string{"environments", "files", "download", "env_abc123", "src/logo.png"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute download default failed: %v", err)
	}
	if gotQuery != "alt=media" {
		t.Errorf("query = %q, want alt=media", gotQuery)
	}
	if got := stdout.String(); got != "./logo.png\n" {
		t.Errorf("stdout = %q, want ./logo.png\\n", got)
	}
	diskBytes, err := os.ReadFile(filepath.Join(workDir, "logo.png"))
	if err != nil {
		t.Fatalf("ReadFile default: %v", err)
	}
	if !bytes.Equal(diskBytes, binaryContent) {
		t.Errorf("downloaded bytes mismatch: got %v, want %v", diskBytes, binaryContent)
	}

	// 2. --out <existing-directory> writes <dir>/<basename>.
	outDir := filepath.Join(t.TempDir(), "downloads")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	root2, stdout2, _ := newTestEnvironmentFilesRoot(srv.URL)
	root2.SetArgs([]string{"environments", "files", "download", "environments/env_abc123", "/src/logo.png", "--out", outDir})
	if err := root2.Execute(); err != nil {
		t.Fatalf("Execute download to dir failed: %v", err)
	}
	wantDirOut := filepath.Join(outDir, "logo.png")
	if got := strings.TrimSpace(stdout2.String()); got != wantDirOut {
		t.Errorf("stdout = %q, want %q", got, wantDirOut)
	}
	dirDiskBytes, err := os.ReadFile(wantDirOut)
	if err != nil || !bytes.Equal(dirDiskBytes, binaryContent) {
		t.Errorf("ReadFile(%q) = %v, %v", wantDirOut, dirDiskBytes, err)
	}
}

func TestEnvironmentFilesValidationAndDirectoryErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/environments/env_abc123/files/src":
			// Server returning a directory listing JSON or 400 directory error.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"files":[{"name":"environments/env_abc123/files/src/main.py","path":"src/main.py","is_directory":false}]}`))
		case "/v1beta/environments/env_abc123/files/dir400":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":400,"message":"path \"dir400\" is a directory","status":"FAILED_PRECONDITION"}}`))
		case "/v1beta/environments/env_abc123/files/missing.txt":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"file not found","status":"NOT_FOUND"}}`))
		default:
			t.Errorf("unexpected server call for invalid input: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	validationCases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			name:    "upload missing all args",
			args:    []string{"environments", "files", "upload"},
			wantSub: "missing environment",
		},
		{
			name:    "upload missing local file",
			args:    []string{"environments", "files", "upload", "env_abc123"},
			wantSub: "missing local file path",
		},
		{
			name:    "upload invalid environment id",
			args:    []string{"environments", "files", "upload", "environments/a/b", "nonexistent.txt"},
			wantSub: `invalid environment id "environments/a/b"`,
		},
		{
			name:    "download missing all args",
			args:    []string{"environments", "files", "download"},
			wantSub: "missing environment",
		},
		{
			name:    "download missing path",
			args:    []string{"environments", "files", "download", "env_abc123"},
			wantSub: "missing file path",
		},
		{
			name:    "download trailing slash directory rejected locally",
			args:    []string{"environments", "files", "download", "env_abc123", "src/"},
			wantSub: "list --environment env_abc123 --path src --recursive",
		},
		{
			name:    "download server directory listing rejected with list --recursive hint",
			args:    []string{"environments", "files", "download", "env_abc123", "src"},
			wantSub: "list --environment env_abc123 --path src --recursive",
		},
		{
			name:    "download server 400 directory error rejected with list --recursive hint",
			args:    []string{"environments", "files", "download", "env_abc123", "dir400"},
			wantSub: "list --environment env_abc123 --path dir400 --recursive",
		},
	}

	for _, tc := range validationCases {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := newTestEnvironmentFilesRoot(srv.URL)
			root.SetArgs(tc.args)
			rawErr := root.Execute()
			if rawErr == nil {
				t.Fatalf("expected error for %v, got nil", tc.args)
			}
			err := output.CLIError(root, rawErr)
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantSub)
			}
			if got := clierrors.ExitCode(err); got != clierrors.ExitUsage {
				t.Errorf("ExitCode = %d, want %d (ExitUsage)", got, clierrors.ExitUsage)
			}
		})
	}

	// Verify 404 missing file is distinguished from a directory error (returns ExitRuntime / exit 1).
	t.Run("download missing file 404", func(t *testing.T) {
		root, _, _ := newTestEnvironmentFilesRoot(srv.URL)
		root.SetArgs([]string{"environments", "files", "download", "env_abc123", "missing.txt"})
		rawErr := root.Execute()
		err := output.CLIError(root, rawErr)
		if got := clierrors.ExitCode(err); got != clierrors.ExitRuntime {
			t.Errorf("404 ExitCode = %d, want %d (ExitRuntime)", got, clierrors.ExitRuntime)
		}
	})
}

func TestAttachInteractionInputsAndNormalizeFilesURIs(t *testing.T) {
	dir := t.TempDir()
	localImg := filepath.Join(dir, "photo.png")
	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47}
	if err := os.WriteFile(localImg, imgBytes, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	wantB64 := base64.StdEncoding.EncodeToString(imgBytes)

	var filesGetCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1beta/files/abc123" {
			filesGetCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"name":"files/abc123","uri":"` + "https://generativelanguage.googleapis.com/v1beta/files/abc123" + `","mimeType":"image/jpeg","state":"ACTIVE"}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	newTestTree := func(gotBody *string, gotArgs *[]string) *cobra.Command {
		root, _, _ := newTestEnvironmentFilesRoot(srv.URL)
		makeIntent := func(name string) *cobra.Command {
			cmd := &cobra.Command{
				Use:  name + " <prompt>",
				Args: cobra.ArbitraryArgs,
				RunE: func(c *cobra.Command, args []string) error {
					*gotArgs = append([]string(nil), args...)
					if flagutil.FlagChanged(c, "body") {
						*gotBody, _ = c.Flags().GetString("body")
					} else if flagutil.FlagChanged(c, "body-param") {
						*gotBody, _ = c.Flags().GetString("body-param")
					}
					return nil
				},
			}
			cmd.Flags().String("body", "", "")
			cmd.Flags().StringP("body-param", "b", "", "")
			cmd.Flags().String("out", "", "")
			cmd.Flags().Bool("raw-response", false, "")
			cmd.Flags().Bool("schema", false, "")
			return cmd
		}
		root.AddCommand(makeIntent("generate"), makeIntent("image"), makeIntent("video"))

		agentGroup := &cobra.Command{Use: "agent"}
		runCmd := &cobra.Command{
			Use:  "run [input]",
			Args: cobra.ArbitraryArgs,
			RunE: func(c *cobra.Command, args []string) error {
				*gotArgs = append([]string(nil), args...)
				if flagutil.FlagChanged(c, "body") {
					*gotBody, _ = c.Flags().GetString("body")
				}
				return nil
			},
		}
		runCmd.Flags().String("body", "", "")
		runCmd.Flags().Bool("schema", false, "")
		agentGroup.AddCommand(runCmd)
		root.AddCommand(agentGroup)

		if err := attachInteractionInputs(root); err != nil {
			t.Fatalf("attachInteractionInputs: %v", err)
		}
		return root
	}

	t.Run("image -i local file with prompt encodes inline base64 and appends text block", func(t *testing.T) {
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"image", "remove the background", "-i", localImg})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if len(gotArgs) != 0 {
			t.Errorf("gotArgs = %v, want empty (folded into body)", gotArgs)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
			t.Fatalf("Unmarshal body %q: %v", gotBody, err)
		}
		inputs, _ := parsed["input"].([]any)
		if len(inputs) != 2 {
			t.Fatalf("input len = %d, want 2: %v", len(inputs), parsed["input"])
		}
		imgPart, _ := inputs[0].(map[string]any)
		if imgPart["type"] != "image" || imgPart["mime_type"] != "image/png" || imgPart["data"] != wantB64 {
			t.Errorf("imgPart = %+v, want image/png with base64 data", imgPart)
		}
		txtPart, _ := inputs[1].(map[string]any)
		if txtPart["type"] != "text" || txtPart["text"] != "remove the background" {
			t.Errorf("txtPart = %+v, want text prompt", txtPart)
		}
	})

	t.Run("video -b with short files/<id> resolves URI and mime_type via FilesGet", func(t *testing.T) {
		filesGetCalls = 0
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"video", "-b", `{"input":[{"type":"image","uri":"files/abc123"},{"type":"text","text":"animate this"}]}`})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if filesGetCalls != 1 {
			t.Errorf("filesGetCalls = %d, want 1", filesGetCalls)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
			t.Fatalf("Unmarshal body %q: %v", gotBody, err)
		}
		inputs, _ := parsed["input"].([]any)
		imgPart, _ := inputs[0].(map[string]any)
		if imgPart["uri"] != "https://generativelanguage.googleapis.com/v1beta/files/abc123" {
			t.Errorf("uri = %v, want full v1beta files URI", imgPart["uri"])
		}
		if imgPart["mime_type"] != "image/jpeg" {
			t.Errorf("mime_type = %v, want image/jpeg", imgPart["mime_type"])
		}
	})

	t.Run("video -b with full Files API URI missing mime_type populates mime_type via FilesGet", func(t *testing.T) {
		filesGetCalls = 0
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"video", "-b", `{"input":[{"type":"image","uri":"https://generativelanguage.googleapis.com/v1beta/files/abc123"},{"type":"text","text":"animate this"}]}`})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if filesGetCalls != 1 {
			t.Errorf("filesGetCalls = %d, want 1", filesGetCalls)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
			t.Fatalf("Unmarshal body %q: %v", gotBody, err)
		}
		inputs, _ := parsed["input"].([]any)
		imgPart, _ := inputs[0].(map[string]any)
		if imgPart["mime_type"] != "image/jpeg" {
			t.Errorf("mime_type = %v, want image/jpeg", imgPart["mime_type"])
		}
	})

	t.Run("generate -b with short files/<id> under --dry-run expands URI without FilesGet", func(t *testing.T) {
		filesGetCalls = 0
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"generate", "--dry-run", "-b", `{"input":[{"type":"image","uri":"files/abc123"}]}`})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if filesGetCalls != 0 {
			t.Errorf("filesGetCalls = %d, want 0 under --dry-run", filesGetCalls)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
			t.Fatalf("Unmarshal body %q: %v", gotBody, err)
		}
		inputs, _ := parsed["input"].([]any)
		imgPart, _ := inputs[0].(map[string]any)
		if imgPart["uri"] != "https://generativelanguage.googleapis.com/v1beta/files/abc123" {
			t.Errorf("uri = %v, want full v1beta files URI", imgPart["uri"])
		}
	})

	t.Run("agent run -i files/<id> with prompt resolves remote file and appends text block", func(t *testing.T) {
		filesGetCalls = 0
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"agent", "run", "describe this", "-i", "files/abc123"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if filesGetCalls != 1 {
			t.Errorf("filesGetCalls = %d, want 1", filesGetCalls)
		}
		if len(gotArgs) != 0 {
			t.Errorf("gotArgs = %v, want empty (folded into body)", gotArgs)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
			t.Fatalf("Unmarshal body %q: %v", gotBody, err)
		}
		inputs, _ := parsed["input"].([]any)
		if len(inputs) != 2 {
			t.Fatalf("input len = %d, want 2: %v", len(inputs), parsed["input"])
		}
		imgPart, _ := inputs[0].(map[string]any)
		if imgPart["uri"] != "https://generativelanguage.googleapis.com/v1beta/files/abc123" || imgPart["mime_type"] != "image/jpeg" {
			t.Errorf("imgPart = %+v, want resolved URI and image/jpeg", imgPart)
		}
		txtPart, _ := inputs[1].(map[string]any)
		if txtPart["type"] != "text" || txtPart["text"] != "describe this" {
			t.Errorf("txtPart = %+v, want text prompt", txtPart)
		}
	})

	t.Run("invalid files/<id> in -b fails with ExitUsage and zero network requests", func(t *testing.T) {
		filesGetCalls = 0
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"video", "-b", `{"input":[{"type":"image","uri":"files/../bad"}]}`})
		rawErr := root.Execute()
		if rawErr == nil {
			t.Fatal("expected error for invalid files URI")
		}
		err := output.CLIError(root, rawErr)
		if got := clierrors.ExitCode(err); got != clierrors.ExitUsage {
			t.Errorf("ExitCode = %d, want %d (ExitUsage)", got, clierrors.ExitUsage)
		}
		if filesGetCalls != 0 {
			t.Errorf("filesGetCalls = %d, want 0", filesGetCalls)
		}
	})

	t.Run("full Files API URI in -i resolves via FilesGet and rejects invalid file IDs", func(t *testing.T) {
		filesGetCalls = 0
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"generate", "summarize", "-i", "https://generativelanguage.googleapis.com/v1beta/files/abc123"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if filesGetCalls != 1 {
			t.Errorf("filesGetCalls = %d, want 1", filesGetCalls)
		}

		rootBad := newTestTree(&gotBody, &gotArgs)
		rootBad.SetArgs([]string{"generate", "summarize", "-i", "https://generativelanguage.googleapis.com/v1beta/files/bad:id"})
		if err := rootBad.Execute(); err == nil {
			t.Fatal("expected error for invalid full Files API URI")
		}
	})

	t.Run("merging -i with array and object body input", func(t *testing.T) {
		for _, bodyJSON := range []string{
			`{"input":[{"type":"text","text":"existing array item"}]}`,
			`{"input":{"type":"text","text":"existing map item"}}`,
		} {
			var gotBody string
			var gotArgs []string
			root := newTestTree(&gotBody, &gotArgs)
			root.SetArgs([]string{"generate", "-i", localImg, "-b", bodyJSON})
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute(%s) failed: %v", bodyJSON, err)
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
				t.Fatalf("Unmarshal body %q: %v", gotBody, err)
			}
			inputs, _ := parsed["input"].([]any)
			if len(inputs) != 2 {
				t.Errorf("input len for %s = %d, want 2: %v", bodyJSON, len(inputs), parsed["input"])
			}
		}
	})

	t.Run("custom --server-url full Files API URI and dry-run expansion", func(t *testing.T) {
		filesGetCalls = 0
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"--server-url", srv.URL, "generate", "summarize", "-i", srv.URL + "/v1beta/files/abc123"})
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute custom server-url -i failed: %v", err)
		}
		if filesGetCalls != 1 {
			t.Errorf("filesGetCalls = %d, want 1", filesGetCalls)
		}

		rootDry := newTestTree(&gotBody, &gotArgs)
		rootDry.SetArgs([]string{"--server-url", "https://custom.example.com", "--dry-run", "generate", "-b", `{"input":[{"type":"image","uri":"files/abc123"}]}`})
		if err := rootDry.Execute(); err != nil {
			t.Fatalf("Execute custom server-url --dry-run failed: %v", err)
		}
		if !strings.Contains(gotBody, "https://custom.example.com/v1beta/files/abc123") {
			t.Errorf("dry-run body = %s, want custom server-url expansion", gotBody)
		}
	})

	t.Run("conflicting positional prompt, body input, and --input returns usage error", func(t *testing.T) {
		var gotBody string
		var gotArgs []string
		root := newTestTree(&gotBody, &gotArgs)
		root.SetArgs([]string{"generate", "extra prompt", "-i", localImg, "-b", `{"input":"body prompt"}`})
		rawErr := root.Execute()
		if rawErr == nil {
			t.Fatal("expected error when combining positional prompt, --input, and body.input")
		}
		err := output.CLIError(root, rawErr)
		if got := clierrors.ExitCode(err); got != clierrors.ExitUsage {
			t.Errorf("ExitCode = %d, want %d (ExitUsage)", got, clierrors.ExitUsage)
		}
	})
}

func TestResolveEnvUploadAndDownloadArgsCoverage(t *testing.T) {
	if _, _, extra := resolveEnvUploadArgs([]string{"extra"}, "env1", "file1"); !extra {
		t.Error("resolveEnvUploadArgs with both flags and positional arg: want extra=true")
	}
	if _, _, extra := resolveEnvDownloadArgs([]string{"extra"}, "env1", "path1"); !extra {
		t.Error("resolveEnvDownloadArgs with both flags and positional arg: want extra=true")
	}
	for _, sub := range []string{"upload", "download"} {
		root, _, _ := newTestEnvironmentFilesRoot("http://127.0.0.1:0")
		_ = root.PersistentFlags().Set("usage", "true")
		cmd := findChild(findChild(findChild(root, "environments"), "files"), sub)
		if err := cmd.Args(cmd, nil); err != nil {
			t.Errorf("%s Args with --usage returned error: %v", sub, err)
		}
	}
}

func TestUploadEnvironmentChunksBoundaryAndUnexpectedEOF(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk, _ := io.ReadAll(r.Body)
		received = append(received, chunk...)
		w.Header().Set("X-Goog-Upload-Status", "final")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	root, _, _ := newTestEnvironmentFilesRoot(srv.URL)
	root.SetContext(t.Context())
	tr, err := newRawTransport(root)
	if err != nil {
		t.Fatalf("newRawTransport: %v", err)
	}

	// Reader has more bytes than totalSize; uploadEnvironmentChunks must read exactly totalSize bytes.
	if _, err := uploadEnvironmentChunks(root, tr, srv.URL+"/upload", strings.NewReader("0123456789EXTRA"), 10, "text/plain"); err != nil {
		t.Fatalf("uploadEnvironmentChunks exact boundary failed: %v", err)
	}
	if string(received) != "0123456789" {
		t.Errorf("received = %q, want \"0123456789\"", string(received))
	}

	// Premature EOF (empty reader with totalSize > 0) must return an error instead of looping.
	if _, err := uploadEnvironmentChunks(root, tr, srv.URL+"/upload", strings.NewReader(""), 10, "text/plain"); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("uploadEnvironmentChunks premature EOF error = %v, want unexpected EOF", err)
	}
}


