/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

var (
	tag               = ""
	target            = ""
	configFile        = ""
	githubUser        = os.Getenv("GITHUB_USER")
	githubPassword    = os.Getenv("GITHUB_PASSWORD")
	githubAccessToken = os.Getenv("GITHUB_TOKEN")
)

type Config struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`

	Assets []AssetMapping `json:"assets"`
}

type AssetMapping struct {
	Source     string `json:"source"`
	GithubName string `json:"githubName"`
	Optional   bool   `json:"optional"`
}

func main() {
	flag.StringVar(&tag, "tag", "", "tag to push as release")
	flag.StringVar(&target, "target", "", "commitish value that determines where the tag is created from")
	flag.StringVar(&configFile, "config", "", "config file to use")
	buildDir, err := os.Getwd()
	if err != nil {
		glog.Fatalf("error getting current directory: %v", err)
	}
	flag.StringVar(&buildDir, "builddir", buildDir, "directory in which we have built code (default current directory)")
	flag.Set("logtostderr", "true")
	flag.Parse()

	ctx := context.Background()

	if tag == "" {
		glog.Fatalf("must specify -tag")
	}

	if configFile == "" {
		glog.Fatalf("must specify -config")
	}

	configBytes, err := ioutil.ReadFile(configFile)
	if err != nil {
		glog.Fatalf("error reading config file %q: %v", configFile, err)
	}

	config := &Config{}
	if err := yaml.Unmarshal(configBytes, config); err != nil {
		glog.Fatalf("error parsing config file %q: %v", configFile, err)
	}

	shipbot := &Shipbot{
		Config: config,
	}

	{
		if githubAccessToken != "" {
			source := oauth2.StaticTokenSource(&oauth2.Token{
				AccessToken: githubAccessToken,
			})
			shipbot.Client = github.NewClient(oauth2.NewClient(ctx, source))

		} else if githubUser != "" && githubPassword != "" {
			transport := &github.BasicAuthTransport{
				Username: githubUser,
				Password: githubPassword,
			}
			shipbot.Client = github.NewClient(transport.Client())

		} else {
			glog.Fatalf("unable to find github credentials")
		}
	}

	if err := shipbot.DoRelease(ctx, buildDir); err != nil {
		glog.Fatalf("unexpected error: %v", err)
	}
}

type Shipbot struct {
	Client *github.Client
	Config *Config
}

func (sb *Shipbot) DoRelease(ctx context.Context, buildDir string) error {
	glog.Infof("listing github releases for %s/%s", sb.Config.Owner, sb.Config.Repo)
	releases, _, err := sb.Client.Repositories.ListReleases(ctx, sb.Config.Owner, sb.Config.Repo, &github.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing releases: %v", err)
	}

	var found *github.RepositoryRelease
	for _, release := range releases {
		if sv(release.TagName) == tag {
			glog.Infof("found release: %v", sv(release.TagName))
			found = release
		}
	}

	if found == nil {
		if target == "" {
			target, err = findCommitSha(buildDir, tag)
			if err != nil {
				return fmt.Errorf("cannot find sha for tag %q: %v", tag, err)
			}
		}

		glog.Infof("target commitish: %s", target)
		release := &github.RepositoryRelease{
			TagName:         s(tag),
			TargetCommitish: s(target),
			Name:            s(tag),
			Body:            s("Release " + tag + " (draft)"),
			Draft:           b(true),
		}

		glog.Infof("creating github release for %s/%s/%s", sb.Config.Owner, sb.Config.Repo, tag)
		found, _, err = sb.Client.Repositories.CreateRelease(ctx, sb.Config.Owner, sb.Config.Repo, release)
		if err != nil {
			return fmt.Errorf("error creating release: %v", err)
		}
	}

	glog.Infof("listing github release assets for %s/%s/%s", sb.Config.Owner, sb.Config.Repo, tag)
	assets, _, err := sb.Client.Repositories.ListReleaseAssets(ctx, sb.Config.Owner, sb.Config.Repo, i64v(found.ID), &github.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing assets: %v", err)
	}

	assetMap := make(map[string]*github.ReleaseAsset)
	for _, asset := range assets {
		assetMap[sv(asset.Name)] = asset
	}

	for i := range sb.Config.Assets {
		assetMapping := &sb.Config.Assets[i]
		err := sb.syncAsset(ctx, found, assetMapping, assetMap)
		if err != nil {
			return err
		}
	}

	return nil
}

func findCommitSha(basedir string, tag string) (string, error) {
	cmd := exec.Command("git", "rev-list", "-n", "1", tag)
	cmd.Dir = basedir
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error getting git sha @%q: %v", tag, err)
	}
	sha := strings.TrimSpace(out.String())
	if len(sha) != 40 {
		return "", fmt.Errorf("git sha had unexpected length: %q", sha)
	}
	return sha, nil
}

func (sb *Shipbot) syncAsset(ctx context.Context, release *github.RepositoryRelease, assetMapping *AssetMapping, assets map[string]*github.ReleaseAsset) error {
	srcStat, err := os.Stat(assetMapping.Source)
	if err != nil {
		if !assetMapping.Optional {
			return fmt.Errorf("error doing stat %q: %v", assetMapping.Source, err)
		}

		return nil // ignore not found errors
	}

	existing := assets[assetMapping.GithubName]
	if existing != nil {
		// TODO: Fetch asset to see if we can get the SHA (maybe an etag?)

		if int64(iv(existing.Size)) != srcStat.Size() {
			// TODO: Support force-replace mode?
			return fmt.Errorf("asset %q size did not match", assetMapping.GithubName)
		} else {
			glog.Infof("asset sizes match; assuming the same for %s", assetMapping.GithubName)
			return nil
		}
	}

	f, err := os.Open(assetMapping.Source)
	if err != nil {
		return fmt.Errorf("error opening %q: %v", assetMapping.Source, err)
	}
	defer f.Close()

	uploadOptions := &github.UploadOptions{
		Name: assetMapping.GithubName,
	}

	glog.Infof("creating github release assets for %s/%s/%s %q", sb.Config.Owner, sb.Config.Repo, tag, assetMapping.GithubName)
	abs, err := filepath.Abs(assetMapping.Source)
	if err != nil {
		glog.V(2).Infof("error getting absolute path for %q: %v", assetMapping.Source, err)
		abs = assetMapping.Source
	}
	glog.Infof("uploading %q", abs)
	asset, _, err := sb.Client.Repositories.UploadReleaseAsset(ctx, sb.Config.Owner, sb.Config.Repo, i64v(release.ID), uploadOptions, f)
	if err != nil {
		return fmt.Errorf("error uploading assets %q: %v", assetMapping.GithubName, err)
	}

	glog.Infof("uploaded asset: %v", asset)
	return nil
}

func sv(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func iv(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func i64v(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func s(v string) *string {
	return &v
}

func b(v bool) *bool {
	return &v
}
