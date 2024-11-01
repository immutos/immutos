/*
 * Copyright 2024 Damian Peckett <damian@pecke.tt>.
 *
 * Licensed under the Immutos Community Edition License, Version 1.0
 * (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 *    http://immutos.com/licenses/LICENSE-1.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrg/xdg"
	"github.com/containerd/containerd/platforms"
	"github.com/dpeckett/deb822/types/arch"
	"github.com/dpeckett/telemetry"
	"github.com/dpeckett/telemetry/v1alpha1"
	"github.com/gregjones/httpcache"
	"github.com/immutos/immutos/internal/buildkit"
	"github.com/immutos/immutos/internal/constants"
	"github.com/immutos/immutos/internal/database"
	"github.com/immutos/immutos/internal/recipe"
	latestrecipe "github.com/immutos/immutos/internal/recipe/v1alpha1"
	"github.com/immutos/immutos/internal/resolve"
	"github.com/immutos/immutos/internal/secondstage"
	"github.com/immutos/immutos/internal/source"
	"github.com/immutos/immutos/internal/types"
	"github.com/immutos/immutos/internal/unpack"
	"github.com/immutos/immutos/internal/util"
	"github.com/immutos/immutos/internal/util/diskcache"
	"github.com/immutos/immutos/internal/util/hashreader"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli/v2"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/sync/errgroup"
)

func main() {
	defaultCacheDir, _ := xdg.CacheFile("immutos")
	defaultStateDir, _ := xdg.StateFile("immutos")

	persistentFlags := []cli.Flag{
		&cli.GenericFlag{
			Name:  "log-level",
			Usage: "Set the log verbosity level",
			Value: util.FromSlogLevel(slog.LevelInfo),
		},
		&cli.StringFlag{
			Name:   "cache-dir",
			Usage:  "Directory to store the cache",
			Value:  defaultCacheDir,
			Hidden: true,
		},
		&cli.StringFlag{
			Name:   "state-dir",
			Usage:  "Directory to store application state",
			Value:  defaultStateDir,
			Hidden: true,
		},
	}

	initLogger := func(c *cli.Context) error {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: (*slog.Level)(c.Generic("log-level").(*util.LevelFlag)),
		})))

		return nil
	}

	initCacheDir := func(c *cli.Context) error {
		cacheDir := c.String("cache-dir")
		if cacheDir == "" {
			return fmt.Errorf("no cache directory specified")
		}

		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			return fmt.Errorf("failed to create cache directory: %w", err)
		}

		return nil
	}

	initStateDir := func(c *cli.Context) error {
		stateDir := c.String("state-dir")
		if stateDir == "" {
			return fmt.Errorf("no state directory specified")
		}

		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return fmt.Errorf("failed to create state directory: %w", err)
		}

		return nil
	}

	// Collect anonymized usage statistics.
	var telemetryReporter *telemetry.Reporter

	initTelemetry := func(c *cli.Context) error {
		telemetryReporter = telemetry.NewReporter(c.Context, slog.Default(), telemetry.Configuration{
			BaseURL: constants.TelemetryURL,
			Tags:    []string{"immutos"},
		})

		// Some basic system information.
		info := map[string]string{
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
			"num_cpu": fmt.Sprintf("%d", runtime.NumCPU()),
			"version": constants.Version,
		}

		telemetryReporter.ReportEvent(&v1alpha1.TelemetryEvent{
			Kind:   v1alpha1.TelemetryEventKindInfo,
			Name:   "ApplicationStart",
			Values: info,
		})

		return nil
	}

	shutdownTelemetry := func(c *cli.Context) error {
		if telemetryReporter == nil {
			return nil
		}

		telemetryReporter.ReportEvent(&v1alpha1.TelemetryEvent{
			Kind: v1alpha1.TelemetryEventKindInfo,
			Name: "ApplicationStop",
		})

		// Don't want to block the shutdown of the application for too long.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		if err := telemetryReporter.Shutdown(ctx); err != nil {
			slog.Error("Failed to close telemetry reporter", slog.Any("error", err))
		}

		return nil
	}

	app := &cli.App{
		Name:    "immutos",
		Usage:   "Debian images that'll run anywhere",
		Version: constants.Version,
		Commands: []*cli.Command{
			{
				Name:  "build",
				Usage: "Build a Debian base system image",
				Flags: append([]cli.Flag{
					&cli.StringFlag{
						Name:     "filename",
						Aliases:  []string{"f"},
						Usage:    "Recipe file to use",
						Required: true,
					},
					&cli.StringFlag{
						Name:    "output",
						Aliases: []string{"o"},
						Usage:   "Output OCI image archive",
						Value:   "debian-image.tar",
					},
					&cli.StringFlag{
						Name:    "platform",
						Aliases: []string{"p"},
						Usage:   "Target platform(s) in the 'os/arch' format",
						Value:   "linux/" + runtime.GOARCH,
					},
					&cli.StringSliceFlag{
						Name:    "tag",
						Aliases: []string{"t"},
						Usage:   "Name and optionally a tag for the image in the 'name:tag' format",
						Value:   cli.NewStringSlice(),
					},
					&cli.BoolFlag{
						Name:  "dev",
						Usage: "Enable development mode",
					},
				}, persistentFlags...),
				Before: util.BeforeAll(initLogger, initCacheDir, initStateDir, initTelemetry),
				After:  shutdownTelemetry,
				Action: func(c *cli.Context) error {
					// Cache all HTTP responses on disk.
					cache, err := diskcache.NewDiskCache(c.String("cache-dir"), "http")
					if err != nil {
						return fmt.Errorf("failed to create disk cache: %w", err)
					}

					// Use the disk cache for all HTTP requests.
					http.DefaultClient = &http.Client{
						Transport: httpcache.NewTransport(cache),
					}

					// A temporary directory used during image building.
					tempDir, err := os.MkdirTemp("", "immutos-*")
					if err != nil {
						return fmt.Errorf("failed to create temporary directory: %w", err)
					}
					defer func() {
						_ = os.RemoveAll(tempDir)
					}()

					// Mutual TLS certificates for the BuildKit daemon.
					certsDir := filepath.Join(c.String("state-dir"), "certs")
					if err := os.MkdirAll(certsDir, 0o700); err != nil {
						return fmt.Errorf("failed to create certs directory: %w", err)
					}

					// Load the recipe file.
					recipeFile, err := os.Open(c.String("filename"))
					if err != nil {
						return fmt.Errorf("failed to open recipe file: %w", err)
					}
					defer recipeFile.Close()

					rx, err := recipe.FromYAML(recipeFile)
					if err != nil {
						return fmt.Errorf("failed to read recipe: %w", err)
					}

					// Start the BuildKit daemon.
					b := buildkit.New("immutos", certsDir)
					if err := b.StartDaemon(c.Context); err != nil {
						return fmt.Errorf("failed to start buildkit daemon: %w", err)
					}

					// If running in development mode, use the current immutos binary as the
					// second stage binary.
					var secondStageBinaryPath string
					if c.Bool("dev") {
						secondStageBinaryPath, err = os.Executable()
						if err != nil {
							return fmt.Errorf("failed to get executable path: %w", err)
						}
					}

					var downloadOnly bool
					if rx.Options != nil {
						downloadOnly = rx.Options.DownloadOnly
					}

					buildOpts := buildkit.BuildOptions{
						OCIArchivePath:        c.String("output"),
						RecipePath:            c.String("filename"),
						SecondStageBinaryPath: secondStageBinaryPath,
						DownloadOnly:          downloadOnly,
						ImageConf:             toOCIImageConfig(rx),
						Tags:                  c.StringSlice("tag"),
					}

					for _, platformStr := range strings.Split(c.String("platform"), ",") {
						platform, err := platforms.Parse(platformStr)
						if err != nil {
							return fmt.Errorf("failed to parse platform: %w", err)
						}

						if platform.OS != "linux" {
							return fmt.Errorf("unsupported OS: %s", platform.OS)
						}

						slog.Info("Building image", slog.String("platform", platforms.Format(platform)))

						slog.Info("Loading packages")

						var packageDB *database.PackageDB
						packageDB, sourceDateEpoch, err := loadPackageDB(c.Context, rx, platform)
						if err != nil {
							return err
						}

						if sourceDateEpoch.After(buildOpts.SourceDateEpoch) {
							buildOpts.SourceDateEpoch = sourceDateEpoch
						}

						var requiredNameVersions []string

						// By default, install the immutos binary (for second-stage provisioning).
						if !c.Bool("dev") {
							requiredNameVersions = append(requiredNameVersions, "immutos")
						}

						// By default, install all priority required packages.
						if !(rx.Options != nil && rx.Options.OmitRequired) {
							_ = packageDB.ForEach(func(pkg types.Package) error {
								if pkg.Priority == "required" {
									requiredNameVersions = append(requiredNameVersions, pkg.Package.Name)
								}

								return nil
							})
						}

						slog.Info("Resolving selected packages")

						selectedDB, err := resolve.Resolve(packageDB,
							append(requiredNameVersions, rx.Packages.Include...),
							rx.Packages.Exclude)
						if err != nil {
							return err
						}

						platformTempDir := filepath.Join(tempDir, strings.ReplaceAll(platforms.Format(platform), "/", "-"))
						if err := os.MkdirAll(platformTempDir, 0o755); err != nil {
							return fmt.Errorf("failed to create platform temp directory: %w", err)
						}

						slog.Info("Downloading selected packages")

						packagePaths, err := downloadSelectedPackages(c.Context, platformTempDir, selectedDB)
						if err != nil {
							return err
						}

						slog.Info("Unpacking packages")

						dpkgDatabaseArchivePath, dataArchivePaths, err := unpack.Unpack(c.Context, platformTempDir, packagePaths)
						if err != nil {
							return err
						}

						buildOpts.PlatformOpts = append(buildOpts.PlatformOpts, buildkit.PlatformBuildOptions{
							Platform:                platform,
							BuildContextDir:         platformTempDir,
							DpkgDatabaseArchivePath: dpkgDatabaseArchivePath,
							DataArchivePaths:        dataArchivePaths,
						})
					}

					slog.Info("Building multi-platform image", slog.String("output", c.String("output")))

					if err := b.Build(c.Context, buildOpts); err != nil {
						return fmt.Errorf("failed to build OCI image: %w", err)
					}

					return nil
				},
			},
			{
				Name:        "second-stage",
				Description: "Operations that will be run after the image is built",
				Hidden:      true,
				Subcommands: []*cli.Command{
					{
						// MergeUsr is a separate command as it needs to be run before
						// packages are configured.
						Name:        "merge-usr",
						Description: "Merge the /usr directory into the root filesystem",
						Flags:       persistentFlags,
						Before:      util.BeforeAll(initLogger),
						Action: func(_ *cli.Context) error {
							return secondstage.MergeUsr()
						},
					},
					{
						Name:        "provision",
						Description: "Set up the image with the requested recipe",
						Flags: append([]cli.Flag{
							&cli.StringFlag{
								Name:     "filename",
								Aliases:  []string{"f"},
								Usage:    "Recipe file to use",
								Required: true,
							},
						}, persistentFlags...),
						Before: util.BeforeAll(initLogger),
						Action: func(c *cli.Context) error {
							// Load the recipe file.
							recipeFile, err := os.Open(c.String("filename"))
							if err != nil {
								return fmt.Errorf("failed to open recipe file: %w", err)
							}
							defer recipeFile.Close()

							rx, err := recipe.FromYAML(recipeFile)
							if err != nil {
								return fmt.Errorf("failed to read recipe: %w", err)
							}

							return secondstage.Provision(c.Context, rx)
						},
					},
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		slog.Error("Error", slog.Any("error", err))
		os.Exit(1)
	}
}

func loadPackageDB(ctx context.Context, rx *latestrecipe.Recipe, platform ocispecs.Platform) (*database.PackageDB, time.Time, error) {
	var componentsMu sync.Mutex
	var components []source.Component

	var progressOutput io.Writer = os.Stdout
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		progressOutput = io.Discard
	}

	progress := mpb.NewWithContext(ctx, mpb.WithOutput(progressOutput))
	defer progress.Shutdown()

	{
		sourceConfs := append([]latestrecipe.SourceConfig{}, rx.Sources...)

		g, ctx := errgroup.WithContext(ctx)

		bar := progress.AddBar(int64(len(sourceConfs)),
			mpb.PrependDecorators(
				decor.Name("Source: "),
				decor.CountersNoUnit("%d / %d"),
			),
			mpb.AppendDecorators(
				decor.Percentage(),
			),
		)

		for _, sourceConf := range sourceConfs {
			sourceConf := sourceConf

			g.Go(func() error {
				defer bar.Increment()

				s, err := source.NewSource(ctx, sourceConf)
				if err != nil {
					return fmt.Errorf("failed to create source: %w", err)
				}

				targetArch, err := arch.Parse(platform.Architecture)
				if err != nil {
					return fmt.Errorf("failed to parse target architecture: %w", err)
				}

				sourceComponents, err := s.Components(ctx, targetArch)
				if err != nil {
					return fmt.Errorf("failed to get components: %w", err)
				}

				componentsMu.Lock()
				components = append(components, sourceComponents...)
				componentsMu.Unlock()

				return nil
			})
		}

		err := g.Wait()

		if err != nil {
			bar.Abort(true)
		} else {
			bar.SetTotal(bar.Current(), true)
		}
		bar.Wait()

		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to get components: %w", err)
		}
	}

	packageDB := database.NewPackageDB()

	var sourceDateEpoch time.Time
	{
		g, ctx := errgroup.WithContext(ctx)

		bar := progress.AddBar(int64(len(components)),
			mpb.PrependDecorators(
				decor.Name("Repository: "),
				decor.CountersNoUnit("%d / %d"),
			),
			mpb.AppendDecorators(
				decor.Percentage(),
			),
		)

		for _, component := range components {
			component := component

			g.Go(func() error {
				defer bar.Increment()

				componentPackages, lastUpdated, err := component.Packages(ctx)
				if err != nil {
					return fmt.Errorf("failed to get packages: %w", err)
				}

				if lastUpdated.After(sourceDateEpoch) {
					sourceDateEpoch = lastUpdated
				}

				packageDB.AddAll(componentPackages)

				return nil
			})
		}

		err := g.Wait()

		if err != nil {
			bar.Abort(true)
		} else {
			bar.SetTotal(bar.Current(), true)
		}
		bar.Wait()

		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to get packages: %w", err)
		}
	}

	return packageDB, sourceDateEpoch, nil
}

func downloadSelectedPackages(ctx context.Context, tempDir string, selectedDB *database.PackageDB) ([]string, error) {
	var progressOutput io.Writer = os.Stdout
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		progressOutput = io.Discard
	}

	progress := mpb.NewWithContext(ctx, mpb.WithOutput(progressOutput))
	defer progress.Shutdown()

	bar := progress.AddBar(int64(selectedDB.Len()),
		mpb.PrependDecorators(
			decor.Name("Downloading: "),
			decor.CountersNoUnit("%d / %d"),
		),
		mpb.AppendDecorators(
			decor.Percentage(),
		),
	)

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(10)

	var packagePathsMu sync.Mutex
	var packagePaths []string

	_ = selectedDB.ForEach(func(pkg types.Package) error {
		g.Go(func() error {
			defer bar.Increment()

			var errs error
			for _, pkgURL := range util.Shuffle(pkg.URLs) {
				slog.Debug("Downloading package", slog.String("url", pkgURL))

				packagePath, err := downloadPackage(ctx, tempDir, pkgURL, pkg.SHA256)
				errs = errors.Join(errs, err)
				if err == nil {
					packagePathsMu.Lock()
					packagePaths = append(packagePaths, packagePath)
					packagePathsMu.Unlock()
					errs = nil
					break
				}
			}
			if errs != nil {
				return fmt.Errorf("failed to download package: %w", errs)
			}

			return nil
		})

		return nil
	})

	err := g.Wait()

	if err != nil {
		bar.Abort(true)
	} else {
		bar.SetTotal(bar.Current(), true)
	}
	bar.Wait()

	if err != nil {
		return nil, fmt.Errorf("failed to download packages: %w", err)
	}

	// Sort the package filenames so that they are in a deterministic order.
	slices.Sort(packagePaths)

	return packagePaths, nil
}

func downloadPackage(ctx context.Context, downloadDir, pkgURL, sha256 string) (string, error) {
	url, err := url.Parse(pkgURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse package URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url.String(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download package: %w", err)
	}
	defer resp.Body.Close()

	// Read the package completely so the cache can be populated.
	hr := hashreader.NewReader(resp.Body)

	packageFile, err := os.Create(filepath.Join(downloadDir, filepath.Base(url.Path)))
	if err != nil {
		return "", fmt.Errorf("failed to create package file: %w", err)
	}
	defer packageFile.Close()

	if _, err := io.Copy(packageFile, hr); err != nil {
		_ = packageFile.Close()
		return "", fmt.Errorf("failed to read package: %w", err)
	}

	if err := hr.Verify(sha256); err != nil {
		_ = packageFile.Close()
		return "", fmt.Errorf("failed to verify package: %w", err)
	}

	return packageFile.Name(), nil
}

func toOCIImageConfig(rx *latestrecipe.Recipe) ocispecs.ImageConfig {
	if rx.Container == nil {
		return ocispecs.ImageConfig{}
	}

	return ocispecs.ImageConfig{
		User:         rx.Container.User,
		ExposedPorts: rx.Container.ExposedPorts,
		Env:          rx.Container.Env,
		Entrypoint:   rx.Container.Entrypoint,
		Cmd:          rx.Container.Cmd,
		Volumes:      rx.Container.Volumes,
		WorkingDir:   rx.Container.WorkingDir,
		Labels:       rx.Container.Labels,
		StopSignal:   rx.Container.StopSignal,
	}
}
