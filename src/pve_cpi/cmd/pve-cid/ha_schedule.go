package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The patterns of the replication schedules whose period is plain. PVE reads a
// schedule as a systemd calendar event.
var (
	scheduleEveryMinutes = regexp.MustCompile(`^\*/(\d{1,3})$`)
	scheduleHourlySteps  = regexp.MustCompile(`^\*:(?:\d{1,2}|\*)/(\d{1,3})$`)
	scheduleHourlyAt     = regexp.MustCompile(`^\*:\d{1,2}$`)
	scheduleDailyAt      = regexp.MustCompile(`^\d{1,2}:\d{2}$`)
)

// scheduleInterval returns the longest gap a replication schedule leaves
// between two runs, and reports false for a schedule whose gap it cannot state.
// It reads the keywords, a step of minutes ("*/15"), a step inside every hour
// ("*:0/30"), one run each hour ("*:15"), and one run each day ("02:00").
// A schedule that names weekdays, dates, or lists is not judged, because a
// wrong gap would make the doctor call a healthy job stale or miss a stale one.
func scheduleInterval(spec string) (time.Duration, bool) {
	s := strings.ToLower(strings.TrimSpace(spec))
	switch s {
	case "":
		return 0, false
	case "*", "minutely":
		return time.Minute, true
	case "hourly":
		return time.Hour, true
	case "daily":
		return 24 * time.Hour, true
	case "weekly":
		return 7 * 24 * time.Hour, true
	case "monthly":
		return 31 * 24 * time.Hour, true
	case "quarterly":
		return 92 * 24 * time.Hour, true
	case "semiannually", "semi-annually":
		return 184 * 24 * time.Hour, true
	case "yearly", "annually":
		return 366 * 24 * time.Hour, true
	}
	if m := scheduleEveryMinutes.FindStringSubmatch(s); m != nil {
		return minutes(m[1])
	}
	if m := scheduleHourlySteps.FindStringSubmatch(s); m != nil {
		return minutes(m[1])
	}
	if scheduleHourlyAt.MatchString(s) {
		return time.Hour, true
	}
	if scheduleDailyAt.MatchString(s) {
		return 24 * time.Hour, true
	}
	return 0, false
}

// minutes converts a step of minutes to a duration. A step of zero is not a
// schedule.
func minutes(step string) (time.Duration, bool) {
	n, err := strconv.Atoi(step)
	if err != nil || n <= 0 {
		return 0, false
	}
	return time.Duration(n) * time.Minute, true
}
