package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	cp "github.com/otiai10/copy"
)

const (
	defaultLogLocation = "/Users/jclarke/Desktop/go-photo-import.log"
	defaultSrcDir      = "/Users/jclarke/Desktop/Import"
	defaultDstDir      = "/Users/jclarke/Desktop/modified_go"
)

func main() {
	if runtime.GOOS != "darwin" {
		_, err := fmt.Fprintln(os.Stderr, "This CLI is only supported on MacOS "+
			"due to the use of the SetFile binary to properly maintain the birthtime of files.")
		if err != nil {
			panic(err)
		}
		os.Exit(1)
	}

	// set up flags for CLI
	var (
		logPath string
		srcDir  string
		dstDir  string
		dryRun  bool
		workers int
	)
	flag.StringVar(&logPath, "log-path", defaultLogLocation, "Path to log file")
	flag.StringVar(&srcDir, "src-dir", defaultSrcDir, "Path to source directory")
	flag.StringVar(&dstDir, "dst-dir", defaultDstDir, "Path to destination directory")
	flag.BoolVar(&dryRun, "dry-run", false, "Perform a dry run")
	flag.IntVar(&workers, "workers", 10, "Number of concurrent workers")
	flag.Parse()

	// Set up logger
	cleanup, loggerSetupErr := setupLogger(logPath, dryRun)
	defer cleanup()
	if loggerSetupErr != nil {
		panic(loggerSetupErr)
	}

	// quick check to make sure that `SetFile` exists at the expected location
	if _, setFileExecErr := os.Stat("/usr/bin/SetFile"); errors.Is(setFileExecErr, os.ErrNotExist) {
		msg := "SetFile binary not found, please run `xcode-select --install`"
		slog.Error(msg, "error", setFileExecErr)
		_, _ = fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
	slog.Info("SetFile binary found", "path", "/usr/bin/SetFile")
	slog.Info("Source directory", "dir_path", srcDir)
	slog.Info("Destination directory", "dir_path", dstDir)
	slog.Info("Number of workers", "count", workers)

	// validate the src dir exists
	if _, statErr := os.Stat(srcDir); errors.Is(statErr, os.ErrNotExist) {
		msg := "Source directory not found"
		slog.Error(msg, "error", statErr)
		_, _ = fmt.Fprintln(os.Stderr, msg, srcDir)
		os.Exit(1)
	}

	// Locate photos in the source directory
	photosFound, locateErr := locatePhotos(srcDir, dstDir)
	if locateErr != nil {
		panic(locateErr)
	}
	if len(photosFound) == 0 {
		slog.Info("No photos found, exiting", "src_dir", srcDir)
		return
	}
	slog.Info("Located photos", "count", len(photosFound))

	// perform concurrent copy
	start := time.Now()
	copiedCount, copyErr := copyPhotos(photosFound, workers, dryRun)
	if copyErr != nil {
		slog.Error("Error copying photos", "error", copyErr, "success_count", copiedCount)
		os.Exit(1)
	}
	slog.Info("Successfully copied photos", "success_count", copiedCount, "located_count", len(photosFound),
		"elapsed_time", time.Since(start), "average_per_item", time.Since(start)/time.Duration(copiedCount))
}

// configure a logger for writing to a given file, providing a proper cleanup function
func setupLogger(logPath string, dryRun bool) (func(), error) {
	// dynamically build up log file name if non-dry-run so that each run is a different filename, based on a timestamp
	if !dryRun {
		logPath = path.Join(path.Dir(logPath), fmt.Sprintf("%s_%s.log",
			strings.TrimSuffix(path.Base(logPath), ".log"), getTimestamp()))
	}
	// write to stderr to tell user where log is being written
	_, _ = fmt.Fprintln(os.Stderr, "Writing log to", logPath)

	file, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if closeErr := file.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error closing log file: %v\n", closeErr)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(file, nil)))
	return cleanup, nil
}

func getTimestamp() string {
	return time.Now().Format("20060102_150405")
}

// locatePhotos returns a map of file paths to file info for all files in a given directory
func locatePhotos(srcDir, dstDir string) ([]photoInfo, error) {
	var fileData []photoInfo
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// effectively continue past these conditions, since other files will still be walked
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") ||
			strings.HasSuffix(filepath.Ext(d.Name()), ".AAE") {
			return nil
		}
		// get modified time
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}

		// extract system-specific data pointer to birth time of file
		var birthTime time.Time // default to zero value in case the below assertion doesn't work
		statStruct, ok := info.Sys().(*syscall.Stat_t)
		if ok {
			birthTime = time.Unix(statStruct.Birthtimespec.Sec, statStruct.Birthtimespec.Nsec)
		}

		// determine final destination of file
		var (
			year, month, _ = info.ModTime().Date()
			dstDateDirName = filepath.Join(dstDir, strconv.Itoa(year), fmt.Sprintf("%d-%02d", year, month))
			copiedFilePath = filepath.Join(dstDateDirName, filepath.Base(path))
		)

		// add info to our slice
		fileData = append(fileData, photoInfo{
			origPath:      path,
			origModTime:   info.ModTime(),
			origBirthTime: birthTime,
			destPath:      copiedFilePath,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}
	return fileData, nil
}

// photoInfo is a struct that makes the channel add / receive operations easier to follow.
type photoInfo struct {
	origPath      string
	origModTime   time.Time
	origBirthTime time.Time
	destPath      string
}

// copyPhotos concurrently copies all photos in a given directory to a new directory,
// creating each destination directory if it doesn't exist. The destination directory
// is structured as YYYY/YYYY-MM/.
func copyPhotos(photos []photoInfo, workerCount int, dryRun bool) (int32, error) {
	var (
		ch                = make(chan photoInfo, len(photos))
		errs              []error
		mu                = new(sync.RWMutex)
		queuedCount       = new(atomic.Int32)
		copiedCount       = new(atomic.Int32)
		dryRunDirsCreated = make(map[string]struct{})
	)

	// add to queue each photo and start processing immediately since this is being run concurrently.
	go func(photos []photoInfo, ch chan photoInfo) {
		for _, photoDetails := range photos {
			ch <- photoDetails
			slog.Info("Queued photo", "src_path", photoDetails.origPath, "dst_path", photoDetails.destPath)
			queuedCount.Add(1)
		}
		close(ch)
		slog.Info("Queued photos", "count", queuedCount.Load())
	}(photos, ch)

	var wg = new(sync.WaitGroup)
	for i := 0; i < workerCount; i++ {
		wg.Go(func() {
			for photo := range ch {
				// verify that the dir(s) are created before attempting to copy each file to its respective location
				if !dryRun {
					if mkDirErr := os.MkdirAll(filepath.Dir(photo.destPath), 0755); mkDirErr != nil {
						mu.Lock()
						errs = append(errs, mkDirErr)
						mu.Unlock()
						return
					}

					// perform copy of this file to the destination location
					copyStartTime := time.Now()
					if copyErr := cp.Copy(photo.origPath, photo.destPath, cp.Options{PreserveTimes: true}); copyErr != nil {
						mu.Lock()
						errs = append(errs, copyErr)
						mu.Unlock()
						return
					}
					slog.Info("Copied photo", "worker_id", i, "src_path", photo.origPath,
						"dst_path", photo.destPath, "elapsed_time", time.Since(copyStartTime))

					// Additionally, need to set the birthtime (b_time) on the output file by shelling out to the MacOS
					// specific binary `SetFile`. This is to avoid using CGO to perform the same action (which is likely
					// faster to do at scale, but is far more complex and is still CGO).
					setFileStartTime := time.Now()
					if setFileErr := setFileCreationTime(photo.destPath, photo.origBirthTime); setFileErr != nil {
						mu.Lock()
						errs = append(errs, setFileErr)
						mu.Unlock()
						// okay to continue
					}
					slog.Info("Set birthtime", "worker_id", i, "path", photo.destPath,
						"birth_time", photo.origBirthTime, "elapsed_time", time.Since(setFileStartTime))
				} else {
					dstDateDirName := filepath.Dir(photo.destPath)
					if _, statErr := os.Stat(dstDateDirName); errors.Is(statErr, os.ErrNotExist) {
						mu.Lock()
						if _, ok := dryRunDirsCreated[dstDateDirName]; !ok {
							slog.Info("DRY RUN creating dir", "dir_path", dstDateDirName)
							dryRunDirsCreated[dstDateDirName] = struct{}{}
						}
						mu.Unlock()
					}
					slog.Info("DRY RUN copy photo", "worker_id", i, "src_path", photo.origPath, "dst_path", photo.destPath)
				}
				// common to both dry run and non-dry run
				copiedCount.Add(1)
			}
		})
	}
	wg.Wait()

	// join errors if any
	if len(errs) > 0 {
		return copiedCount.Load(), errors.Join(errs...)
	}
	return copiedCount.Load(), nil
}

// setFileCreationTime sets the birthtime (b_time) on a given file.
func setFileCreationTime(filePath string, birthTime time.Time) error {
	var (
		dateFmt = birthTime.Format("01/02/2006 15:04:05")
		cmd     = exec.Command("SetFile", "-d", dateFmt, filePath)
	)
	return cmd.Run()
}
