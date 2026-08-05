// Package scriptwrap executes user-authored inline script handlers (Python, Node)
// inside the workspace sandbox. The handler defines a `handler(input) -> dict`
// function; this package generates a thin per-language wrapper that imports the
// handler, feeds it JSON via stdin, and serializes the return value to stdout.
//
// SECURITY: Execute MUST only be called inside a workspace pod. The handler
// source is user-authored and runs with the workspace user's full privileges
// (filesystem, git, network egress, materialized secrets). Calling Execute
// outside the workspace sandbox is RCE-as-a-service. The package performs no
// handler-source validation, no CPU/memory limits, and no output-size cap —
// those concerns are the caller's responsibility.
package scriptwrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Language is the runtime used to execute the handler.
type Language string

const (
	// LanguagePython executes the handler via python3 (mise-installed).
	LanguagePython Language = "python"
	// LanguageNode executes the handler via node (mise-installed).
	LanguageNode Language = "node"
)

const pythonWrapper = `import json, sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from handler import handler
_input = json.loads(sys.stdin.read())
_result = handler(_input)
sys.stdout.write(json.dumps(_result))
`

const nodeWrapper = `const h = require('./handler');
const _input = JSON.parse(require('fs').readFileSync(0, 'utf8'));
const _result = h.handler(_input);
process.stdout.write(JSON.stringify(_result));
`

// Execute runs the handler in the given language with the JSON-marshaled input
// on stdin and returns the wrapper's stdout. The caller is responsible for
// validating that stdout is a JSON object (dict) — Execute does not enforce
// dict returns; a handler returning a string, number, or array will succeed.
//
// On non-zero exit, returns the captured stderr and exitCode alongside err.
// On context cancellation, the child process is killed (SIGKILL via
// exec.CommandContext) and err wraps ctx.Err(). Note: only the direct child
// is killed; grandchildren spawned by the handler (subprocess.Popen,
// child_process.spawn) may leak. v2 may add process-group killing.
func Execute(ctx context.Context, language Language, handlerSource string, input any) (output json.RawMessage, stderr string, exitCode int, err error) {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, "", -1, fmt.Errorf("marshal input: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "scriptwrap-*")
	if err != nil {
		return nil, "", -1, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	var handlerFile, wrapperFile, wrapperSource, command string
	switch language {
	case LanguagePython:
		handlerFile = "handler.py"
		wrapperFile = "_wrapper.py"
		wrapperSource = pythonWrapper
		command = "python3"
	case LanguageNode:
		handlerFile = "handler.js"
		wrapperFile = "_wrapper.js"
		wrapperSource = nodeWrapper
		command = "node"
	default:
		return nil, "", -1, fmt.Errorf("unsupported language: %s", language)
	}

	if err := os.WriteFile(filepath.Join(tmpDir, handlerFile), []byte(handlerSource), 0o600); err != nil {
		return nil, "", -1, fmt.Errorf("write handler: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, wrapperFile), []byte(wrapperSource), 0o600); err != nil {
		return nil, "", -1, fmt.Errorf("write wrapper: %w", err)
	}

	//nolint:gosec // G204: command is one of two fixed strings (python3/node), not user input. Handler source is user-authored by design (script nodes); confinement is the workspace sandbox (design D2).
	cmd := exec.CommandContext(ctx, command, wrapperFile)
	cmd.Dir = tmpDir
	cmd.Stdin = bytes.NewReader(inputJSON)
	var stdout, stderrBuf bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderrBuf

	err = cmd.Run()
	stderr = stderrBuf.String()
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	if err != nil {
		if ctx.Err() != nil {
			return nil, stderr, exitCode, fmt.Errorf("execution canceled or timed out: %w", ctx.Err())
		}
		return nil, stderr, exitCode, fmt.Errorf("execution failed (exit %d): %w", exitCode, err)
	}

	return json.RawMessage(stdout.Bytes()), stderr, exitCode, nil
}
