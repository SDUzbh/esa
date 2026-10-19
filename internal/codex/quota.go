package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"esa/internal/esa"
)

var (
	usageLimit   = regexp.MustCompile(`(?i)usage limit|rate limit|quota exceeded|too many requests`)
	retryAtClock = regexp.MustCompile(`(?i)try again at\s+(\d{1,2}):(\d{2})\s*([ap]\.?m\.?)?`)
	retryIn      = regexp.MustCompile(`(?i)(?:try again|retry) in\s+(\d+)\s*(second|sec|minute|min|hour|hr|day)s?`)
)

// usageLimited reports whether a Codex CLI failure is a usage or rate limit
// rather than a problem with the run.
func usageLimited(message string) bool {
	return usageLimit.MatchString(message)
}

// quotaReset reads when the limit resets from the CLI's message: a clock time
// in the provider host's zone ("try again at 11:19 PM") or a duration ("in 3
// hours"). Without one it retries in an hour. The result is between one
// minute and seven days away.
func quotaReset(message string, now time.Time) time.Time {
	at := now.Add(time.Hour)
	if m := retryAtClock.FindStringSubmatch(message); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute, _ := strconv.Atoi(m[2])
		meridiem := strings.ToLower(strings.ReplaceAll(m[3], ".", ""))
		if meridiem == "pm" && hour < 12 {
			hour += 12
		} else if meridiem == "am" && hour == 12 {
			hour = 0
		}
		if hour < 24 && minute < 60 {
			local := now.Local()
			at = time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, local.Location())
			if !at.After(now) {
				at = at.AddDate(0, 0, 1)
			}
		}
	} else if m := retryIn.FindStringSubmatch(message); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"second": time.Second, "sec": time.Second, "minute": time.Minute, "min": time.Minute,
			"hour": time.Hour, "hr": time.Hour, "day": 24 * time.Hour}[strings.ToLower(m[2])]
		at = now.Add(time.Duration(n) * unit)
	}
	if at.Before(now.Add(time.Minute)) {
		at = now.Add(time.Minute)
	}
	if at.After(now.Add(7 * 24 * time.Hour)) {
		at = now.Add(7 * 24 * time.Hour)
	}
	return at
}

// setAsideAfterQuota lets the same run repeat a stage the provider's usage
// limit refused: the stage's audit is kept under a new name and its attempt
// marker is cleared. Nothing from the refused attempt is reused.
func setAsideAfterQuota(err error, dir string, names ...string) error {
	var quota *esa.ProviderQuotaError
	if !errors.As(err, &quota) {
		return err
	}
	suffix := fmt.Sprintf(".quota-%d", time.Now().UnixNano())
	for _, name := range names {
		path := filepath.Join(dir, name)
		if _, statErr := os.Lstat(path); statErr != nil {
			continue
		}
		if renameErr := os.Rename(path, path+suffix); renameErr != nil {
			// The stage stays blocked; report that rather than a retryable limit.
			return fmt.Errorf("usage limit reached, but the refused attempt could not be set aside: %w", renameErr)
		}
	}
	return err
}
