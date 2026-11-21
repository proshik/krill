package backup

import "testing"

func TestCronForPreset(t *testing.T) {
	cases := map[string]string{"off": "", "": "", "hourly": "0 * * * *", "daily": "0 3 * * *", "weekly": "0 3 * * 0", "monthly": "0 3 1 * *"}
	for preset, want := range cases {
		got, err := CronForPreset(preset, "")
		if err != nil || got != want {
			t.Errorf("CronForPreset(%q) = (%q,%v), want (%q,nil)", preset, got, err, want)
		}
	}
	if got, err := CronForPreset("custom", "0 5 * * *"); err != nil || got != "0 5 * * *" {
		t.Errorf("custom valid = (%q,%v), want (0 5 * * *,nil)", got, err)
	}
	if _, err := CronForPreset("custom", "not a cron"); err == nil {
		t.Errorf("custom invalid must error")
	}
	if _, err := CronForPreset("bogus", ""); err == nil {
		t.Errorf("unknown preset must error")
	}
}

func TestSchedulePreset(t *testing.T) {
	cases := map[string]string{"": "off", "0 * * * *": "hourly", "0 3 * * *": "daily", "0 3 * * 0": "weekly", "0 3 1 * *": "monthly", "17 2 * * 3": "custom"}
	for sched, want := range cases {
		if got := SchedulePreset(sched); got != want {
			t.Errorf("SchedulePreset(%q) = %q, want %q", sched, got, want)
		}
	}
}

// robfig/cron panics on a timezone prefix with no expression after it.
func TestCronForPresetTimezoneOnly(t *testing.T) {
	for _, c := range []string{"CRON_TZ=UTC", "TZ=UTC", "TZ=Europe/Moscow\t0", "CRON_TZ="} {
		if _, err := CronForPreset("custom", c); err == nil {
			t.Errorf("%q: want an invalid-cron error", c)
		}
	}
	if got, err := CronForPreset("custom", "CRON_TZ=UTC 0 3 * * *"); err != nil || got != "CRON_TZ=UTC 0 3 * * *" {
		t.Errorf("a timezone with an expression: %q, %v", got, err)
	}
}
