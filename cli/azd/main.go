// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/azure/azure-dev/cli/azd/cmd"
	"github.com/azure/azure-dev/cli/azd/internal"
	"github.com/blang/semver/v4"
	"github.com/fatih/color"
	"github.com/spf13/pflag"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if !isDebugEnabled() {
		log.SetOutput(io.Discard)
	}

	latest := make(chan semver.Version)
	go fetchLatestVersion(latest)

	cmdErr := cmd.Execute(os.Args[1:])
	latestVersion, ok := <-latest

	// If we were able to fetch a latest version, check to see if we are up to date and
	// print a warning if we are not. Note that we don't print this warning when the CLI version
	// is exactly 0.0.0-dev.0, which is a sentinel value used for `internal.Version` when
	// a version is not explicitly applied at build time (i.e. dev builds installed with `go install`)
	if ok {
		curVersion, err := semver.Parse(internal.GetVersionNumber())
		if err != nil {
			log.Printf("failed to parse %s as a semver", internal.GetVersionNumber())
		} else if curVersion.Equals(semver.MustParse("0.0.0-dev.0")) {
			// This is a dev build (i.e. built using `go install without setting a version`) - don't print a warning in this case
			log.Printf("eliding update message for dev build")
		} else if latestVersion.GT(curVersion) {
			fmt.Printf(color.YellowString("warning: your version of azd is out of date, you have %s and the latest version is %s\n"), curVersion.String(), latestVersion.String())
			fmt.Println()
			fmt.Println(color.YellowString(`To update to the latest version, run:`))

			if runtime.GOOS == "windows" {
				fmt.Println(color.YellowString(`powershell -c "Set-ExecutionPolicy Bypass Process -Force; irm 'https://aka.ms/install-azd.ps1' | iex"`))
			} else {
				fmt.Println(color.YellowString(`curl -fsSL https://aka.ms/install-azd.sh | bash`))
			}
		}
	}
	if cmdErr != nil {
		os.Exit(1)
	}
}

// azdDirectoryPermissions are the permissions to use on the `.azd` folder in the user's home
// directory.
const azdDirectoryPermissions = 0755

// updateCheckFilePermissions are the permissions to use on the `update-check.json` file.
const updateCheckFilePermissions = 0644

// azdConfigDir is the name of the folder where `azd` writes user wide configuration data.
const azdConfigDir = ".azd"

// updateCheckCacheFileName is the name of the file created in the azd configuration directory
// which is used to cache version information for our up to date check.
const updateCheckCacheFileName = "update-check.json"

// fetchLatestVersion fetches the latest version of the CLI and sends the result
// across the version channel, which it then closes. If the latest version can not
// be determined, the channel is closed without writing a value.
func fetchLatestVersion(version chan<- semver.Version) {
	defer close(version)

	// Allow the user to skip the update check if they wish, by setting AZD_SKIP_UPDATE_CHECK to
	// a truthy value.
	if value, has := os.LookupEnv("AZD_SKIP_UPDATE_CHECK"); has {
		if setting, err := strconv.ParseBool(value); err == nil && setting {
			log.Print("skipping update check since AZD_SKIP_UPDATE_CHECK is true")
			return
		} else if err != nil {
			log.Printf("could not parse value for AZD_SKIP_UPDATE_CHECK a boolean (it was: %s), proceeding with update check", value)
		}
	}

	// To avoid fetching the latest version of the CLI on every invocation, we cache the result for a period
	// of time, in the user's home directory.
	user, err := user.Current()
	if err != nil {
		log.Printf("could not determine current user: %v, skipping update check", err)
		return
	}

	cacheFilePath := filepath.Join(user.HomeDir, azdConfigDir, updateCheckCacheFileName)
	cacheFile, err := os.ReadFile(cacheFilePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("error reading update cache file: %v, skipping update check", err)
		return
	}

	// If we were able to read the update file, try to interpret it and use the cached
	// value if it is still valid. Note the `err == nil` guard here ensures we don't run
	// this logic when the cache file did not exist (since err will be a form of fs.ErrNotExist)
	var cachedLatestVersion *semver.Version
	if err == nil {
		var cache updateCacheFile
		if err := json.Unmarshal(cacheFile, &cache); err == nil {
			parsedVersion, parseVersionErr := semver.Parse(cache.Version)
			parsedExpiresOn, parseExpiresOnErr := time.Parse(time.RFC3339, cache.ExpiresOn)

			if parseVersionErr == nil && parseExpiresOnErr == nil {
				if time.Now().UTC().Before(parsedExpiresOn) {
					log.Printf("using cached latest version: %s (expires on: %s)", cache.Version, cache.ExpiresOn)
					cachedLatestVersion = &parsedVersion
				} else {
					log.Printf("ignoring cached latest version, it is out of date")
				}
			} else {
				if parseVersionErr != nil {
					log.Printf("failed to parse cached version '%s' as a semver: %v, ignoring cached value", cache.Version, parseVersionErr)
				}
				if parseExpiresOnErr != nil {
					log.Printf("failed to parse cached version expiration time '%s' as a RFC3339 timestamp: %v, ignoring cached value", cache.ExpiresOn, parseExpiresOnErr)
				}
			}
		} else {
			log.Printf("could not unmarshal cache file: %v, ignoring cache", err)
		}
	}

	// If we don't have a cached version we can use, fetch one (and cache it)
	if cachedLatestVersion == nil {
		log.Print("fetching latest version information for update check")
		req, err := http.NewRequest(http.MethodGet, "https://aka.ms/azure-dev/versions/cli/latest", nil)
		if err != nil {
			log.Printf("failed to create request object: %v, skipping update check", err)
		}

		req.Header.Set("User-Agent", internal.FormatUserAgent(nil))

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("failed to fetch latest version: %v, skipping update check", err)
			return
		}
		body, err := readToEndAndClose(res.Body)
		if err != nil {
			log.Printf("failed to read response body: %v, skipping update check", err)
			return
		}

		if res.StatusCode != http.StatusOK {
			log.Printf("failed to refresh latest version, http status: %v, body: %v, skipping update check", res.StatusCode, body)
			return
		}

		// Parse the body of the response as a semver, and if it's valid, cache it.
		fetchedVersionText := strings.TrimSpace(body)
		fetchedVersion, err := semver.Parse(fetchedVersionText)
		if err != nil {
			log.Printf("failed to parse latest version '%s' as a semver: %v, skipping update check", fetchedVersionText, err)
			return
		}

		cachedLatestVersion = &fetchedVersion

		// Write the value back to the cache. Note that on these logging paths for errors we do not return
		// eagerly, since we have not yet sent the latest versions across the channel (and we don't want to do that until we've updated
		// the cache since reader on the other end of the channel will exit the process after it receives this value and finishes
		// the up to date check, possibly while this go-routine is still running)
		if err := os.MkdirAll(filepath.Dir(cacheFilePath), azdDirectoryPermissions); err != nil {
			log.Printf("failed to create cache folder '%s': %v", filepath.Dir(cacheFilePath), err)
		} else {
			cacheObject := updateCacheFile{
				Version:   fetchedVersionText,
				ExpiresOn: time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
			}

			// The marshal call can not fail, so we ignore the error.
			cacheContents, _ := json.Marshal(cacheObject)

			if err := os.WriteFile(cacheFilePath, cacheContents, updateCheckFilePermissions); err != nil {
				log.Printf("failed to write update cache file: %v", err)
			} else {
				log.Printf("updated cache file to version %s (expires on: %s)", cacheObject.Version, cacheObject.ExpiresOn)
			}
		}
	}

	// Publish our value, the defer above will close the channel.
	version <- *cachedLatestVersion
}

type updateCacheFile struct {
	// The semver of the  latest version the CLI
	Version string `json:"version"`
	// A time at which this cached value expires, stored as an RFC3339 timestamp
	ExpiresOn string `json:"expiresOn"`
}

// isDebugEnabled checks to see if `--debug` was passed with a truthy
// value.
func isDebugEnabled() bool {
	debug := false
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)

	// Since we are running this parse logic on the full command line, there may be additional flags
	// which we have not defined in our flag set (but would be defined by whatever command we end up
	// running). Setting UnknownFlags instructs `flags.Parse` to continue parsing the command line
	// even if a flag is not in the flag set (instead of just returning an error saying the flag was not
	// found).
	flags.ParseErrorsWhitelist.UnknownFlags = true
	flags.BoolVar(&debug, "debug", false, "")

	if err := flags.Parse(os.Args[1:]); err != nil {
		log.Printf("could not parse flags: %v", err)
	}
	return debug
}

func readToEndAndClose(r io.ReadCloser) (string, error) {
	defer r.Close()
	var buf strings.Builder
	_, err := io.Copy(&buf, r)
	return buf.String(), err
}