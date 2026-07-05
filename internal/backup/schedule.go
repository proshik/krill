package backup

import (
	"fmt"

	"github.com/robfig/cron/v3"
)

// CronForPreset maps a schedule preset to a cron string. "off" (and "") mean
// on-demand: an empty schedule the scheduler skips. "custom" validates the raw
// cron via the same standard parser the scheduler uses.
func CronForPreset(preset, custom string) (string, error) {
	switch preset {
	case "off", "":
		return "", nil
	case "hourly":
		return "0 * * * *", nil
	case "daily":
		return "0 3 * * *", nil
	case "weekly":
		return "0 3 * * 0", nil
	case "monthly":
		return "0 3 1 * *", nil
	case "custom":
		if _, err := cron.ParseStandard(custom); err != nil {
			return "", fmt.Errorf("invalid cron %q: %w", custom, err)
		}
		return custom, nil
	default:
		return "", fmt.Errorf("unknown schedule preset %q", preset)
	}
}

// SchedulePreset is the reverse of CronForPreset for display: it names the preset
// of a stored schedule ("" -> off; a known cron -> its preset; anything else ->
// custom).
func SchedulePreset(schedule string) string {
	switch schedule {
	case "":
		return "off"
	case "0 * * * *":
		return "hourly"
	case "0 3 * * *":
		return "daily"
	case "0 3 * * 0":
		return "weekly"
	case "0 3 1 * *":
		return "monthly"
	default:
		return "custom"
	}
}
