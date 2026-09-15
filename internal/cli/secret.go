package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Test seams for the two terminal calls. They are variables only so the
// package's own tests can drive the echo-suppressed branch without allocating
// a pty; every shipped build uses golang.org/x/term.
var (
	isTerminal   = func(fd uintptr) bool { return term.IsTerminal(int(fd)) }
	readPassword = func(fd uintptr) ([]byte, error) { return term.ReadPassword(int(fd)) }
)

// readSecretLine prompts on w and reads one line from r, without echoing it
// back to the screen when r is a terminal.
//
// The reason is that the line is a bk_live_ key. Echoed, it lands in the
// terminal's scrollback, in `script` and tmux logs, in a screen recording, and
// in front of anyone watching the onboarding demo this command exists for.
//
// The terminal branch is entered only when r really is a terminal, never
// assumed: `echo "$KEY" | billkit login` and a CI heredoc are ordinary ways to
// use this command, and term.ReadPassword fails outright on a pipe. Anything
// that is not a terminal keeps the plain buffered read, which has nothing to
// echo in the first place.
func readSecretLine(r io.Reader, w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)

	if f, ok := r.(*os.File); ok && isTerminal(f.Fd()) {
		b, err := readPassword(f.Fd())
		// The Enter that ended the line was consumed without being echoed,
		// so the cursor is still sitting after the prompt. Everything the
		// command prints next would run onto that line without this.
		fmt.Fprintln(w)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}

	line, err := bufio.NewReader(r).ReadString('\n')
	key := strings.TrimSpace(line)
	// A pipe whose last line has no trailing newline (`printf %s "$KEY" |`,
	// or a secret file saved without one) returns the content alongside EOF.
	// Treating that as a failure would reject a key that arrived intact.
	if err != nil && (!errors.Is(err, io.EOF) || key == "") {
		return "", err
	}
	return key, nil
}
