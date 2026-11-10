package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Jellyfin and plugins ask ffmpeg about itself (-version, -hwaccels,
// -encoders, -h filter=..., ...) at startup, sometimes with short timeouts
// (IntroSkipper allows 2 seconds for -version). The answers never change for
// a given ffmpeg build, so the agent runs each such command once per
// executable and answers later requests from memory.

// infoFlags print information about ffmpeg itself and need no input.
var infoFlags = map[string]bool{
	"-version": true, "-buildconf": true, "-hwaccels": true, "-encoders": true,
	"-decoders": true, "-filters": true, "-codecs": true, "-formats": true,
	"-muxers": true, "-demuxers": true, "-bsfs": true, "-protocols": true,
	"-pix_fmts": true, "-sample_fmts": true, "-layouts": true, "-colors": true,
	"-devices": true, "-L": true,
}

var helpTopics = []string{"filter=", "encoder=", "decoder=", "bsf=", "muxer=", "demuxer=", "protocol="}

// isInfoCommand reports whether args only ask ffmpeg about itself.
func isInfoCommand(args []string) bool {
	info := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case infoFlags[a]:
			info = true
		case a == "-hide_banner" || a == "-nostdin":
		case a == "-v" || a == "-loglevel":
			i++ // skip the level
		case a == "-h" || a == "-help" || a == "--help":
			info = true
			if i+1 < len(args) {
				for _, t := range helpTopics {
					if strings.HasPrefix(args[i+1], t) {
						i++
						break
					}
				}
			}
		default:
			return false // anything else (an input, an option): a real job
		}
	}
	return info
}

type infoResult struct {
	stdout, stderr []byte
	code           int
	err            error
}

type infoEntry struct {
	once sync.Once
	res  infoResult
}

// infoCache runs each info command once per ffmpeg executable.
type infoCache struct {
	mu      sync.Mutex
	entries map[string]*infoEntry
	run     func(ctx context.Context, exe string, args []string) infoResult
}

func newInfoCache() *infoCache {
	return &infoCache{entries: map[string]*infoEntry{}, run: runInfoCommand}
}

// get returns the (possibly cached) output. Failed runs are not kept.
func (c *infoCache) get(exe string, args []string) infoResult {
	key := exe + "\x00" + strings.Join(args, "\x00")
	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		e = &infoEntry{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		e.res = c.run(ctx, exe, args)
	})
	if e.res.err != nil || e.res.code != 0 {
		c.mu.Lock()
		if c.entries[key] == e {
			delete(c.entries, key) // retry next time
		}
		c.mu.Unlock()
	}
	return e.res
}

func runInfoCommand(ctx context.Context, exe string, args []string) infoResult {
	cmd := exec.CommandContext(ctx, exe, args...)
	setProcAttr(cmd)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	res := infoResult{stdout: out.Bytes(), stderr: errOut.Bytes()}
	if cmd.ProcessState != nil {
		res.code = cmd.ProcessState.ExitCode()
		err = nil
	}
	res.err = err
	return res
}
