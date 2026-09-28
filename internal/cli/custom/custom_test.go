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
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/google-gemini/gemini-api-cli/internal/flagutil"
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
			desc:      "Model id, e.g. gemini-2.5-flash",
			normalize: normalizeModelPositional,
			args:      []string{"models/gemini-2.5-flash"},
			wantVal:   "gemini-2.5-flash",
		},
		{
			name:      "models get flag bare",
			flagName:  "model",
			shorthand: "m",
			desc:      "Model id, e.g. gemini-2.5-flash",
			normalize: normalizeModelPositional,
			args:      []string{"--model", "gemini-2.5-flash"},
			wantVal:   "gemini-2.5-flash",
		},
		{
			name:      "models get invalid flag",
			flagName:  "model",
			shorthand: "m",
			desc:      "Model id, e.g. gemini-2.5-flash",
			normalize: normalizeModelPositional,
			args:      []string{"--model", "../files/x"},
			wantErr:   `invalid model id "../files/x"`,
		},
		{
			name:      "models get missing id",
			flagName:  "model",
			shorthand: "m",
			desc:      "Model id, e.g. gemini-2.5-flash",
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

