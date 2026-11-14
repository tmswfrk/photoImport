package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cp "github.com/otiai10/copy"
	"github.com/tmswfrk/photoImport/dateParser"
	"github.com/tmswfrk/photoImport/logger"
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
		logPath   string
		srcDir    string
		dstDir    string
		syncTimes bool
		dryRun    bool
		workers   int
	)
	flag.StringVar(&logPath, "log-path", defaultLogLocation, "Path to log file")
	flag.StringVar(&srcDir, "src-dir", defaultSrcDir, "Path to source directory")
	flag.StringVar(&dstDir, "dst-dir", defaultDstDir, "Path to destination directory")
	flag.BoolVar(&syncTimes, "sync-times", false, "Also sync file modification time to creation time")
	flag.BoolVar(&dryRun, "dry-run", false, "Perform a dry run")
	flag.IntVar(&workers, "workers", 10, "Number of concurrent workers")
	flag.Parse()

	// Set up logger
	cleanup, loggerSetupErr := logger.Setup(logPath, dryRun)
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
	photosFound, locateErr := locatePhotos(srcDir)
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
	copiedCount, copyErr := copyPhotos(photosFound, dstDir, workers, syncTimes, dryRun)
	if copyErr != nil && !dryRun {
		slog.Error("Error copying photos", "error", copyErr, "success_count", copiedCount)
		os.Exit(1)
	}
	slog.Info("Successfully copied photos", "success_count", copiedCount, "located_count", len(photosFound),
		"elapsed_time", time.Since(start), "average_per_item", time.Since(start)/time.Duration(copiedCount))
}

// locatePhotos returns a map of file paths to file info for all files in a given directory
func locatePhotos(srcDir string) (chan string, error) {
	//var fileData []photoInfo
	var fileData []string
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// effectively continue past these conditions, since other files will still be walked
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") ||
			strings.HasSuffix(filepath.Ext(d.Name()), ".AAE") {
			return nil
		}

		// add info to our slice
		fileData = append(fileData, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// return as a read-from channel
	var ch = make(chan string, len(fileData))
	for _, fPath := range fileData {
		ch <- fPath
		slog.Info("Queued photo", "src_path", fPath)
	}
	close(ch)
	return ch, nil
}

// copyPhotos concurrently copies all photos in a given directory to a new directory,
// creating each destination directory if it doesn't exist. The destination directory
// is structured as YYYY/YYYY-MM/.
func copyPhotos(photos <-chan string, dstDir string, workerCount int, syncTimes, dryRun bool) (int32, error) {
	var (
		errs              []error
		mu                = new(sync.RWMutex)
		copiedCount       = new(atomic.Int32)
		dryRunDirsCreated = make(map[string]struct{})
	)

	var wg = new(sync.WaitGroup)
	for i := 0; i < workerCount; i++ {
		wg.Go(func() {
			for photoPath := range photos {
				// verify that the dir(s) are created before attempting to copy each file to its respective location
				metaData, metaDataErr := getExifMetadata(photoPath, dstDir)
				if metaDataErr != nil {
					mu.Lock()
					errs = append(errs, metaDataErr)
					mu.Unlock()
					continue
				}

				if !dryRun {
					if mkDirErr := os.MkdirAll(filepath.Dir(metaData.destPath), 0755); mkDirErr != nil {
						mu.Lock()
						errs = append(errs, mkDirErr)
						mu.Unlock()
						continue
					}

					// perform copy of this file to the destination location
					copyStartTime := time.Now()
					if copyErr := cp.Copy(photoPath, metaData.destPath, cp.Options{PreserveTimes: true}); copyErr != nil {
						mu.Lock()
						errs = append(errs, copyErr)
						mu.Unlock()
						continue
					}
					slog.Info("Copied photo", "worker_id", i, "src_path", photoPath,
						"dst_path", metaData.destPath, "elapsed_time", time.Since(copyStartTime))

					// Additionally, need to set the birthtime (b_time) on the output file by shelling out to the MacOS
					// specific binary `SetFile`. This is to avoid using CGO to perform the same action (which is likely
					// faster to do at scale, but is far more complex and is still CGO).
					setFileStartTime := time.Now()
					if setFileErr := setFileCreationTime(metaData.destPath, metaData.origBirthTime, syncTimes); setFileErr != nil {
						mu.Lock()
						errs = append(errs, setFileErr)
						mu.Unlock()
						// okay to continue
					}
					slog.Info("Set birthtime", "worker_id", i, "path", metaData.destPath,
						"birth_time", metaData.origBirthTime, "elapsed_time", time.Since(setFileStartTime))
				} else {
					dstDateDirName := filepath.Dir(metaData.destPath)
					if _, statErr := os.Stat(dstDateDirName); errors.Is(statErr, os.ErrNotExist) {
						mu.Lock()
						if _, ok := dryRunDirsCreated[dstDateDirName]; !ok {
							slog.Info("DRY RUN creating dir", "dir_path", dstDateDirName)
							dryRunDirsCreated[dstDateDirName] = struct{}{}
						}
						mu.Unlock()
					}
					slog.Info("DRY RUN copy photo", "worker_id", i, "src_path", photoPath, "dst_path", metaData.destPath)
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

type photoMetaData struct {
	destPath      string
	exifDateTime  time.Time
	origBirthTime time.Time
}

// getExifMetadata returns metadata for photoPath, including its birth date and dated destination under dstDir.
// It returns an error if the file cannot be opened, statted, or dated.
func getExifMetadata(photoPath, dstDir string) (*photoMetaData, error) {
	// open the file
	f, openErr := os.Open(photoPath)
	if openErr != nil {
		return nil, openErr
	}
	defer f.Close()

	// also attempt to get the file info from stat'ing the file
	fi, statErr := f.Stat()
	if statErr != nil {
		return nil, statErr
	}

	// Using the above file handle and fileInfo, determine a "birthdate" of this file, preferring
	// exif data if located rather than using a syscall to determine it.
	origBirthTime, bdErr := dateParser.DetermineBirthDate(f, fi)
	if bdErr != nil {
		return nil, bdErr
	}

	// determine final destination of file based on the timestamp found
	var (
		year, month, _ = origBirthTime.Date()
		dstDateDirName = filepath.Join(dstDir, strconv.Itoa(year), fmt.Sprintf("%d-%02d", year, month))
		copiedFilePath = filepath.Join(dstDateDirName, filepath.Base(photoPath))
	)

	return &photoMetaData{
		destPath:      copiedFilePath,
		origBirthTime: origBirthTime,
	}, nil
}

// setFileCreationTime sets the birthtime (b_time) on a given file.
func setFileCreationTime(filePath string, birthTime time.Time, syncTimes bool) error {
	var (
		dateFmt = birthTime.Format("01/02/2006 15:04:05")
		args    = []string{"-d", dateFmt}
	)
	if syncTimes {
		args = append(args, "-m", dateFmt)
	}
	cmd := exec.Command("SetFile", append(args, filePath)...)
	return cmd.Run()
}
