# PhotoImport for Golang
This is a small utility I wrote for combing through my photos taken from my iPhone and organizing them into folders based on their respective year and month.

It uses Golang goroutines via workers so that I/O can be maximized by the host OS. By default, this is `10`, but I've used `50` to really zip through photos / movies. It seems to do a decent job saturating your CPU usage (currently running an M3 Apple Silicon with 16GB of memory).

# Additional Details
Golang was developed on top of C, which means that some of the way this works on Mac is not entirely intuitive. This means:
- I need to utilize another utility like `github.com/otiai10/copy` to perform the file + metadata copy
- I need to use a Mac specific utility, `SetFile` to make sure the `birthdate` on a given file is updated to reflect the metadata.

```
~/some/src/dir
  IMG_1234.jpg
  IMG_1235.jpg
  ...

~/some/dst/dir
  2025/
    2025-01/
      IMG_1234.jpg
    2025-03/
      IMG_1235.jpg
```

Each photo already has `exif` data, but these additions help when navigating photos in a given MacOS filesystem.

Note that the CLI will terminate early if not run from a Darwin host (as determined by `runtime.GOOS`) or if `SetFile` is not available.

# Runtime Info
`make` && `./photo_import -help`
