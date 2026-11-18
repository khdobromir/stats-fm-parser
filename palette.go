package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Values are SGR parameters. "selection" is the only background role.
type palette map[string]string

func terminalPalette() palette {
	return palette{"accent": "36", "selection": "7", "foreground": "39", "muted": "90", "red": "31", "green": "32", "yellow": "33"}
}

func (p palette) paint(role, text string) string {
	if p[role] == "" || text == "" {
		return text
	}
	return "\x1b[" + p[role] + "m" + text + "\x1b[0m"
}
func loadPalette(ctx context.Context) palette {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "omarchy", "theme", "color", "--all").Output()
	if err != nil {
		return terminalPalette()
	}
	return parsePalette(string(data))
}
func parsePalette(output string) palette {
	p := terminalPalette()
	for line := range strings.Lines(output) {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		role, hex := parts[0], parts[1]
		if p[role] == "" || len(hex) != 7 || hex[0] != '#' {
			continue
		}
		n, err := strconv.ParseUint(hex[1:], 16, 24)
		if err != nil {
			continue
		}
		code := 38
		if role == "selection" {
			code = 48
		}
		p[role] = fmt.Sprintf("%d;2;%d;%d;%d", code, n>>16, (n>>8)&255, n&255)
	}
	return p
}
