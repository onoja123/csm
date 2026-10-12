package provider

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// fakeEcho shows, when CSM_FAKE_ECHO is set, which test agent is running and the exact opening instruction csm gave it, so a demo makes no claim beyond what csm delivered.
func fakeEcho(agent, profile, sessionID, instruction string) {
	if os.Getenv("CSM_FAKE_ECHO") == "" {
		return
	}

	if ms, err := strconv.Atoi(os.Getenv("CSM_FAKE_START_MS")); err == nil && ms > 0 && instruction != "" {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}

	if ms, err := strconv.Atoi(os.Getenv("CSM_FAKE_HANDOFF_MS")); err == nil && ms > 0 && strings.HasPrefix(instruction, "You are continuing a coding task") {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}

	fmt.Printf("\x1b[2m%s · csm test agent, no model · profile %s · session %s\x1b[0m\r\n\r\n", agent, profile, shortID(sessionID))

	if instruction == "" {
		fmt.Print("Waiting. This stand-in records the session for csm and does no work.\r\n")

		return
	}

	fmt.Print("Opening instruction received from csm:\r\n\r\n")

	for _, line := range strings.Split(instruction, "\n") {
		fmt.Print("  " + line + "\r\n")
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}

	return id
}

func fakeHold() time.Duration {
	if secs, err := strconv.Atoi(os.Getenv("CSM_FAKE_HOLD_SECONDS")); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}

	return 30 * time.Second
}

func fakeEchoLine(line string) {
	if os.Getenv("CSM_FAKE_ECHO") != "" {
		fmt.Print("\r\n" + line + "\r\n")
	}
}

// fakeLimited says whether a test agent profile hits the simulated usage limit; CSM_FAKE_LIMIT_STEP keeps it to one Build Room step.
func fakeLimited(profile string) bool {
	if !slices.Contains(strings.Split(os.Getenv("CSM_FAKE_LIMIT"), ","), profile) {
		return false
	}

	step := os.Getenv("CSM_FAKE_LIMIT_STEP")

	return step == "" || step == os.Getenv("CSM_BUILD_STEP")
}
