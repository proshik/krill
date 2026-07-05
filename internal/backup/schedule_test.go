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
