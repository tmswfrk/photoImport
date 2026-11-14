package dateParser

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// DetermineBirthDate returns the file's creation time, preferring EXIF DateTime over filesystem birth time.
// It returns an error when neither EXIF data nor filesystem metadata can provide a date.
func DetermineBirthDate(f *os.File, fi os.FileInfo) (time.Time, error) {
	var defaultTime time.Time

	// attempt to retrieve exif data from file
	foundDate, extractErr := extractExifDate(f)
	if extractErr == nil {
		return foundDate, nil
	}

	// fallthrough to syscall approach
	// get birthtime of file from OS syscall
	statStruct, ok := fi.Sys().(*syscall.Stat_t)
	if ok {
		birthtime := time.Unix(statStruct.Birthtimespec.Sec, statStruct.Birthtimespec.Nsec)
		slog.Warn("Using syscall to determine birthtime of file", "file_name", f.Name(),
			"birth_time", birthtime.String())
		return birthtime, nil
	}
	retErr := errors.New("unable to determine date from either EXIF data or a syscall")
	slog.Error(retErr.Error(), "file_name", f.Name())
	return defaultTime, retErr
}

// extractExifDate attempts to extract the EXIF DateTime from the given file.
func extractExifDate(f *os.File) (time.Time, error) {
	metaData, decodeErr := exif.Decode(f)
	if decodeErr == nil {
		dt, dtErr := metaData.DateTime()
		if dtErr == nil {
			// additional check in case the date that comes back is a "dummy" date, in which case
			// we should simply fall back to the syscall process below.
			if !isDummyExifDate(dt.String()) {
				return dt, nil
			}
		}
	}
	// function can return nil, we need to return early here before we panic, will have to rely on syscall
	// to determine the date.
	if metaData == nil {
		return time.Time{}, errors.New("unable to decode EXIF data")
	}

	// Try again using a more manual approach in case we have other, nonstandard exif date formats
	// Fallback: inspect tags manually in order of preference
	dateTags := []exif.FieldName{
		exif.DateTimeOriginal,
		exif.DateTimeDigitized, // "CreateDate" in exiftool
		exif.DateTime,          // "ModifyDate" in exiftool
	}

	customLayouts := []string{
		"01.02.2006 15:04",
		"2006:01:02 15:04:05",
		"2006-01-02 15:04:05",
		"2006:01:02 15:04:05-07:00",
		"2006:01:02 15:04:05Z07:00",
		"2006/01/02 15:04:05",
		"2006:1:2 15:4:5",
	}

	for _, tagName := range dateTags {
		tag, err := metaData.Get(tagName)
		if err != nil {
			continue // Tag isn't present in this file
		}

		raw, err := tag.StringVal()
		if err != nil {
			continue
		}

		// Clean null bytes and surrounding whitespace common in older camera firmware
		cleaned := strings.TrimSpace(strings.TrimRight(raw, "\x00"))
		if cleaned == "" || cleaned == "0000:00:00 00:00:00" {
			continue
		}

		// iterate over each specified layout and return the first that matches
		for _, layout := range customLayouts {
			if t, err := time.ParseInLocation(layout, cleaned, time.Local); err == nil {
				return t, nil
			}
		}
	}
	slog.Warn("Unable to infer EXIF date, falling back to syscall", "file_name", f.Name())
	return time.Time{}, errors.New("unable to determine date from EXIF data")
}

var knownDummyDates = map[string]struct{}{
	"2002:09:30 11:11:11": {},
	"2002:09:30 11:11:00": {},
	"0000:00:00 00:00:00": {},
}

const exifLayout = "2006-01-02 15:04:05 -0700 MST"

func isDummyExifDate(raw string) bool {
	// translate into time.Time
	parsedTime, parseErr := time.Parse(exifLayout, raw)
	if parseErr != nil {
		return false
	}
	if _, isDummy := knownDummyDates[parsedTime.Format("2006:01:02 15:04:05")]; isDummy {
		return true
	}
	return false
}
