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
	"strconv"
	"strings"
)

// audioFormat is the parsed shape of an audio MIME type such as
// "audio/L16;codec=pcm;rate=24000".
type audioFormat struct {
	baseMIME   string
	sampleRate int
	channels   int
	isPCM      bool
}

// parseAudioMIME extracts the base type and PCM parameters from an audio MIME
// type. PCM channel count defaults to 1 (mono) when absent, matching what the
// TTS models return.
func parseAudioMIME(mimeType string) audioFormat {
	parts := strings.Split(mimeType, ";")
	f := audioFormat{baseMIME: strings.ToLower(strings.TrimSpace(parts[0]))}
	for _, p := range parts[1:] {
		key, val, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(val))
		if err != nil || n <= 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "rate", "samplerate", "sample_rate":
			f.sampleRate = n
		case "channels", "channelcount", "channel_count":
			f.channels = n
		}
	}
	switch f.baseMIME {
	case "audio/l16", "audio/pcm", "audio/raw":
		f.isPCM = true
		if f.channels == 0 {
			f.channels = 1
		}
	}
	return f
}

// wavHeader builds a canonical 44-byte RIFF/WAVE header for 16-bit PCM.
func wavHeader(pcmLen, sampleRate, channels int) []byte {
	const bitsPerSample = 16
	blockAlign := channels * bitsPerSample / 8
	byteRate := sampleRate * blockAlign
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+pcmLen))
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(h[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[34:], bitsPerSample)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(pcmLen))
	return h
}

// extensionForAudioMIME picks the artifact extension for a returned audio
// MIME type. Raw PCM is wrapped as WAV, so it maps to .wav. ok is false for a
// type the CLI cannot write as a playable file — headerless companded audio
// (audio/alaw, audio/mulaw) or anything unknown — rather than guessing .wav for
// bytes that are not a WAV.
func extensionForAudioMIME(mimeType string) (ext string, ok bool) {
	format := parseAudioMIME(mimeType)
	if format.isPCM {
		return ".wav", true
	}
	switch format.baseMIME {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav", true
	case "audio/mp3", "audio/mpeg":
		return ".mp3", true
	case "audio/aac":
		return ".aac", true
	case "audio/ogg", "audio/ogg_opus", "audio/vorbis":
		return ".ogg", true
	case "audio/flac":
		return ".flac", true
	case "audio/opus":
		return ".opus", true
	case "audio/m4a", "audio/mp4":
		return ".m4a", true
	}
	return "", false
}
