package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/integration/checker"
	"github.com/go-check/check"
)

var (
	reTimestamp  = `\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}.\d{9}(:?(:?(:?-|\+)\d{2}:\d{2})|Z)`
	reEventType  = `(?P<eventType>\w+)`
	reAction     = `(?P<action>\w+)`
	reID         = `(?P<id>[^\s]+)`
	reAttributes = `(\s\((?P<attributes>[^\)]+)\))?`
	reString     = fmt.Sprintf(`\A%s\s%s\s%s\s%s%s\z`, reTimestamp, reEventType, reAction, reID, reAttributes)

	// eventCliRegexp is a regular expression that matches all possible event outputs in the cli
	eventCliRegexp = regexp.MustCompile(reString)
)

// eventMatcher is a function that tries to match an event input.
type eventMatcher func(text string) bool

// eventObserver runs an events commands and observes its output.
type eventObserver struct {
	buffer             *bytes.Buffer
	command            *exec.Cmd
	scanner            *bufio.Scanner
	startTime          string
	disconnectionError error
}

// newEventObserver creates the observer and initializes the command
// without running it. Users must call `eventObserver.Start` to start the command.
func newEventObserver(c *check.C, args ...string) (*eventObserver, error) {
	since := daemonTime(c).Unix()
	return newEventObserverWithBacklog(c, since, args...)
}

// newEventObserverWithBacklog creates a new observer changing the start time of the backlog to return.
func newEventObserverWithBacklog(c *check.C, since int64, args ...string) (*eventObserver, error) {
	startTime := strconv.FormatInt(since, 10)
	cmdArgs := []string{"events", "--since", startTime}
	if len(args) > 0 {
		cmdArgs = append(cmdArgs, args...)
	}
	eventsCmd := exec.Command(dockerBinary, cmdArgs...)
	stdout, err := eventsCmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	return &eventObserver{
		buffer:    new(bytes.Buffer),
		command:   eventsCmd,
		scanner:   bufio.NewScanner(stdout),
		startTime: startTime,
	}, nil
}

// Start starts the events command.
func (e *eventObserver) Start() error {
	return e.command.Start()
}

// Stop stops the events command.
func (e *eventObserver) Stop() {
	e.command.Process.Kill()
	e.command.Process.Release()
}

// Match tries to match the events output with a given matcher.
func (e *eventObserver) Match(match eventMatcher) {
	for e.scanner.Scan() {
		text := e.scanner.Text()
		e.buffer.WriteString(text)
		e.buffer.WriteString("\n")

		match(text)
	}

	err := e.scanner.Err()
	if err == nil {
		err = io.EOF
	}

	logrus.Debug("EventObserver scanner loop finished: %v", err)
	e.disconnectionError = err
}

func (e *eventObserver) CheckEventError(c *check.C, id, event string, match eventMatcher) {
	var foundEvent bool
	scannerOut := e.buffer.String()

	if e.disconnectionError != nil {
		until := strconv.FormatInt(daemonTime(c).Unix(), 10)
		out, _ := dockerCmd(c, "events", "--since", e.startTime, "--until", until)
		events := strings.Split(strings.TrimSpace(out), "\n")
		for _, e := range events {
			if match(e) {
				foundEvent = true
				break
			}
		}
		scannerOut = out
	}
	if !foundEvent {
		c.Fatalf("failed to observe event `%s` for %s. Disconnection error: %v\nout:\n%v", event, id, e.disconnectionError, scannerOut)
	}
}

// matchEventLine matches a text with the event regular expression.
// It returns the action and true if the regular expression matches with the given id and event type.
// It returns an empty string and false if there is no match.
func matchEventLine(id, eventType string, actions map[string]chan bool) eventMatcher {
	return func(text string) bool {
		matches := parseEventText(text)
		if len(matches) == 0 {
			return false
		}

		if matchIDAndEventType(matches, id, eventType) {
			if ch, ok := actions[matches["action"]]; ok {
				close(ch)
				return true
			}
		}
		return false
	}
}

// parseEventText parses a line of events coming from the cli and returns
// the matchers in a map.
func parseEventText(text string) map[string]string {
	matches := eventCliRegexp.FindAllStringSubmatch(text, -1)
	md := map[string]string{}
	if len(matches) == 0 {
		return md
	}

	names := eventCliRegexp.SubexpNames()
	for i, n := range matches[0] {
		md[names[i]] = n
	}
	return md
}

// parseEventAction parses an event text and returns the action.
// It fails if the text is not in the event format.
func parseEventAction(c *check.C, text string) string {
	matches := parseEventText(text)
	return matches["action"]
}

// eventActionsByIDAndType returns the actions for a given id and type.
// It fails if the text is not in the event format.
func eventActionsByIDAndType(c *check.C, events []string, id, eventType string) []string {
	var filtered []string
	for _, event := range events {
		matches := parseEventText(event)
		c.Assert(matches, checker.Not(checker.IsNil))
		if matchIDAndEventType(matches, id, eventType) {
			filtered = append(filtered, matches["action"])
		}
	}
	return filtered
}

// matchIDAndEventType returns true if an event matches a given id and type.
// It also resolves names in the event attributes if the id doesn't match.
func matchIDAndEventType(matches map[string]string, id, eventType string) bool {
	return matchEventID(matches, id) && matches["eventType"] == eventType
}

func matchEventID(matches map[string]string, id string) bool {
	matchID := matches["id"] == id || strings.HasPrefix(matches["id"], id)
	if !matchID && matches["attributes"] != "" {
		// try matching a name in the attributes
		attributes := map[string]string{}
		for _, a := range strings.Split(matches["attributes"], ", ") {
			kv := strings.Split(a, "=")
			attributes[kv[0]] = kv[1]
		}
		matchID = attributes["name"] == id
	}
	return matchID
}

func parseEvents(c *check.C, out, match string) {
	events := strings.Split(strings.TrimSpace(out), "\n")
	for _, event := range events {
		matches := parseEventText(event)
		matched, err := regexp.MatchString(match, matches["action"])
		c.Assert(err, checker.IsNil)
		c.Assert(matched, checker.True, check.Commentf("Matcher: %s did not match %s", match, matches["action"]))
	}
}

func parseEventsWithID(c *check.C, out, match, id string) {
	events := strings.Split(strings.TrimSpace(out), "\n")
	for _, event := range events {
		matches := parseEventText(event)
		c.Assert(matchEventID(matches, id), checker.True)

		matched, err := regexp.MatchString(match, matches["action"])
		c.Assert(err, checker.IsNil)
		c.Assert(matched, checker.True, check.Commentf("Matcher: %s did not match %s", match, matches["action"]))
	}
}
