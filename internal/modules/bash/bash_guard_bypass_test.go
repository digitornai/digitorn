package bash

import (
	"strings"
	"testing"
)

// =============================================================================
// Guard bypass tests — these prove the bugs identified in the audit, and they
// MUST stay green after the fix. Each "bypass" case asserts the guard BLOCKS
// the destructive command; each "regression" case asserts a legitimate command
// is NOT blocked, so an agent can still execute the most complex shell work.
// =============================================================================

func TestGuard_M2_QuotedFlagsBypassedRegex(t *testing.T) {
	bypasses := []string{
		`rm "-rf" /`,
		`rm '-rf' /`,
		`rm "-rf" /*`,
		`rm "--recursive" --force /`,
	}
	for _, cmd := range bypasses {
		t.Run(cmd, func(t *testing.T) {
			if err := checkCommand(cmd); err == nil {
				t.Fatalf("M2: guard missed quoted-flags bypass: %q", cmd)
			}
		})
	}
}

func TestGuard_G1_LongFlagsBypassedRegex(t *testing.T) {
	bypasses := []string{
		`rm --recursive --force /`,
		`rm --force --recursive /`,
		`rm --recursive --force /*`,
	}
	for _, cmd := range bypasses {
		t.Run(cmd, func(t *testing.T) {
			if err := checkCommand(cmd); err == nil {
				t.Fatalf("G1: guard missed long-flag bypass: %q", cmd)
			}
		})
	}
}

func TestGuard_G3_DdToNonSdDevicesBypassed(t *testing.T) {
	bypasses := []string{
		`dd if=/dev/zero of=/dev/vda`,
		`dd if=/dev/zero of=/dev/xvda`,
		`dd if=/dev/zero of=/dev/mmcblk0`,
		`dd if=/dev/zero of=/dev/loop0`,
		`dd if=/dev/zero of=/dev/mem`,
		`dd if=/dev/zero of=/dev/kmem`,
		`dd if=/dev/zero of=/dev/ram0`,
	}
	for _, cmd := range bypasses {
		t.Run(cmd, func(t *testing.T) {
			if err := checkCommand(cmd); err == nil {
				t.Fatalf("G3: guard missed dangerous-device write: %q", cmd)
			}
		})
	}
}

// =============================================================================
// Non-regression: legitimate commands MUST still pass. This is the user's
// hard constraint — "the agent must be able to execute the most complex shell
// commands". Every line here is a real-world legitimate use.
// =============================================================================

func TestGuard_NoFalsePositives_LegitimateCommands(t *testing.T) {
	allowed := []string{
		// Existing baseline
		`rm -rf ./build`,
		`git status`,
		`go test ./...`,
		`rm file.txt`,

		// Quoted paths that contain spaces / look like options
		`rm "my file.txt"`,
		`rm "/tmp/some path/foo"`,
		`rm '/var/log/app.log'`,

		// Echo / print containing a guarded pattern: NOT destructive
		`echo "rm -rf /"`,
		`echo 'dd of=/dev/sda'`,
		`printf '%s\n' "rm -rf /"`,

		// Long flags on non-destructive commands
		`pip install --upgrade pip`,
		`npm install --save-dev typescript`,
		`go mod tidy`,
		`docker run --rm -v /tmp/data:/data ubuntu echo hi`,
		`tar --create --file=archive.tar ./build`,

		// dd to non-device files / null
		`dd if=test.iso of=./image.bin bs=1M count=10`,
		`dd if=/dev/urandom of=./random.bin bs=4k count=1`,
		`dd if=somefile of=/dev/null`,

		// Pipelines and subshells — complex shell that MUST work
		`find . -name '*.log' | xargs rm`,
		`cat file.txt | awk '{print $1}' | sort | uniq -c`,
		`(cd /tmp && rm -rf cache_dir)`,
		`while read line; do echo "$line"; done < input.txt`,
		`for f in *.go; do gofmt -w "$f"; done`,

		// Heredoc, multiline
		"cat <<EOF\nhello\nworld\nEOF",
		`bash -c 'rm -rf "$HOME/.cache/myapp"'`,

		// rm with -r BUT not -f, or vice versa
		`rm -r ./testdata`,
		`rm -f ./single-file.tmp`,
		`rm -rf ./.git/index.lock`, // git lockfile cleanup, common
	}
	for _, cmd := range allowed {
		t.Run(cmd, func(t *testing.T) {
			if err := checkCommand(cmd); err != nil {
				t.Fatalf("FALSE POSITIVE: guard refused legitimate command %q: %v", cmd, err)
			}
		})
	}
}

// =============================================================================
// Baseline preserved: the cases that ALREADY worked must keep working.
// =============================================================================

func TestGuard_PreservesExistingDestructiveCoverage(t *testing.T) {
	stillBlocked := []string{
		`rm -rf /`,
		`rm -rf /*`,
		`RM -RF /`,
		`:(){ :|:& };:`,
		`mkfs.ext4 /dev/sda`,
		`dd if=/dev/zero of=/dev/sda`,
		`rm -r -f /`,
		`rm -f -r /`,
		`> /dev/sda`,
	}
	for _, cmd := range stillBlocked {
		t.Run(cmd, func(t *testing.T) {
			if err := checkCommand(cmd); err == nil {
				t.Fatalf("REGRESSION: guard now misses %q", cmd)
			}
		})
	}
}

// Marker: make sure the test file references the package surface so it won't
// be silently dead-stripped by an aggressive linter.
var _ = strings.HasPrefix
