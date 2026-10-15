package webdriver

import (
	"os"
	"strings"
	"time"
)

const webImageTimezoneEnv = "GATEWAY_OPENAI_WEB_IMAGES_TIMEZONE"
const defaultWebImageTimezone = "America/New_York"

func loadWebImageTimezone() (*time.Location, error) {
	name := strings.TrimSpace(os.Getenv(webImageTimezoneEnv))
	if name == "" {
		name = defaultWebImageTimezone
	}
	location, err := time.LoadLocation(name)
	if err != nil || name == "Local" {
		return nil, NewError(ErrorKindInternal, "timezone", "invalid "+webImageTimezoneEnv+": expected an IANA timezone or UTC", 0, false)
	}
	return location, nil
}

func webImageTimezoneFields(location *time.Location, now time.Time) (string, int) {
	_, offset := now.In(location).Zone()
	// ChatGPT uses JavaScript getTimezoneOffset: UTC minus local time, in minutes.
	return location.String(), -offset / 60
}

func browserDateString(now time.Time) string {
	zone, _ := now.Zone()
	displayNames := map[string]string{
		"UTC": "Coordinated Universal Time",
		"EST": "Eastern Standard Time", "EDT": "Eastern Daylight Time",
		"CST": "Central Standard Time", "CDT": "Central Daylight Time",
		"MST": "Mountain Standard Time", "MDT": "Mountain Daylight Time",
		"PST": "Pacific Standard Time", "PDT": "Pacific Daylight Time",
	}
	name := ""
	if zone == "UTC" || strings.HasPrefix(now.Location().String(), "America/") {
		name = displayNames[zone]
	} else if now.Location().String() == "Asia/Shanghai" {
		name = "China Standard Time"
	}
	if name == "" {
		name = zone
	}
	return now.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (" + name + ")"
}
