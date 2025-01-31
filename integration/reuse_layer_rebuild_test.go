package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paketo-buildpacks/occam"
	"github.com/paketo-buildpacks/packit/v2/fs"
	"github.com/sclevine/spec"

	. "github.com/onsi/gomega"
)

func testReusingLayerRebuild(t *testing.T, context spec.G, it spec.S) {
	var (
		Expect = NewWithT(t).Expect

		docker occam.Docker
		pack   occam.Pack

		imageIDs map[string]struct{}

		name   string
		source string
	)

	it.Before(func() {
		var err error
		name, err = occam.RandomName()
		Expect(err).NotTo(HaveOccurred())

		docker = occam.NewDocker()
		pack = occam.NewPack()
		imageIDs = map[string]struct{}{}
	})

	it.After(func() {
		for id := range imageIDs {
			Expect(docker.Image.Remove.Execute(id)).To(Succeed())
		}

		Expect(docker.Volume.Remove.Execute(occam.CacheVolumeNames(name))).To(Succeed())
		Expect(os.RemoveAll(source)).To(Succeed())
	})

	context("when an app is rebuilt and composer.lock does not change", func() {
		it("reuses a layer from a previous build", func() {
			var (
				err         error
				logs        fmt.Stringer
				firstImage  occam.Image
				secondImage occam.Image
				thirdImage  occam.Image
			)

			source, err = occam.Source(filepath.Join("testdata", "default_app"))
			Expect(err).NotTo(HaveOccurred())

			build := pack.WithNoColor().Build.
				WithPullPolicy("never").
				WithBuildpacks(buildpacksArray...).
				WithEnv(map[string]string{
					"BP_PHP_SERVER": "nginx",
				})

			firstImage, logs, err = build.Execute(name, source)
			Expect(err).NotTo(HaveOccurred())

			imageIDs[firstImage.ID] = struct{}{}

			Expect(firstImage.Buildpacks).To(HaveLen(7))

			Expect(firstImage.Buildpacks[2].Key).To(Equal(buildpackInfo.Buildpack.ID))
			Expect(firstImage.Buildpacks[2].Layers).To(HaveKey("composer-packages"))

			Expect(logs.String()).To(ContainSubstring("Running 'composer install --no-progress --no-dev'"))

			// Second pack build with BP_RUN_COMPOSER_INSTALL set to false
			context("with BP_RUN_COMPOSER_INSTALL set to false", func() {
				it.Before(func() {
					Expect(os.Setenv("BP_RUN_COMPOSER_INSTALL", "false")).To(Succeed())
				})
				it.After(func() {
					Expect(os.Unsetenv("BP_RUN_COMPOSER_INSTALL")).To(Succeed())
				})
				secondImage, logs, err = build.Execute(name, source)
				Expect(err).NotTo(HaveOccurred())

				imageIDs[secondImage.ID] = struct{}{}

				Expect(secondImage.Buildpacks).To(HaveLen(7))

				Expect(secondImage.Buildpacks[2].Key).To(Equal(buildpackInfo.Buildpack.ID))
				Expect(secondImage.Buildpacks[2].Layers).To(HaveKey("composer-packages"))

				it("it does not run composer install again", func() {
					Expect(logs.String()).NotTo(ContainSubstring("Running 'composer install --no-progress --no-dev'"))
				})
				Expect(logs.String()).To(ContainSubstring(fmt.Sprintf("Reusing cached layer /layers/%s/composer-packages", strings.ReplaceAll(buildpackInfo.Buildpack.ID, "/", "_"))))
				Expect(secondImage.Buildpacks[2].Layers["composer-packages"].SHA).To(Equal(firstImage.Buildpacks[2].Layers["composer-packages"].SHA))
			})

			// Third pack build with BP_RUN_COMPOSER_INSTALL set to true
			context("with BP_RUN_COMPOSER_INSTALL set to true", func() {
				it.Before(func() {
					Expect(os.Setenv("BP_RUN_COMPOSER_INSTALL", "true")).To(Succeed())
				})
				it.After(func() {
					Expect(os.Unsetenv("BP_RUN_COMPOSER_INSTALL")).To(Succeed())
				})
				thirdImage, logs, err = build.Execute(name, source)
				Expect(err).NotTo(HaveOccurred())

				imageIDs[thirdImage.ID] = struct{}{}

				Expect(thirdImage.Buildpacks).To(HaveLen(7))

				Expect(thirdImage.Buildpacks[2].Key).To(Equal(buildpackInfo.Buildpack.ID))
				Expect(thirdImage.Buildpacks[2].Layers).To(HaveKey("composer-packages"))

				it("it does run composer install again", func() {
					Expect(logs.String()).To(ContainSubstring("Running 'composer install --no-progress --no-dev'"))
				})
				Expect(logs.String()).To(ContainSubstring(fmt.Sprintf("Reusing cached layer /layers/%s/composer-packages", strings.ReplaceAll(buildpackInfo.Buildpack.ID, "/", "_"))))

				Expect(thirdImage.Buildpacks[2].Layers["composer-packages"].SHA).To(Equal(firstImage.Buildpacks[2].Layers["composer-packages"].SHA))
			})
		})
	})

	context("when an app is rebuilt and there is a change in composer.lock", func() {
		it("rebuilds the layer", func() {
			var (
				err         error
				logs        fmt.Stringer
				firstImage  occam.Image
				secondImage occam.Image
			)

			source, err = occam.Source(filepath.Join("testdata", "default_app"))
			Expect(err).NotTo(HaveOccurred())

			build := pack.WithNoColor().Build.
				WithPullPolicy("never").
				WithBuildpacks(buildpacksArray...).
				WithEnv(map[string]string{
					"BP_PHP_SERVER": "nginx",
				})

			firstImage, logs, err = build.Execute(name, source)
			Expect(err).NotTo(HaveOccurred())

			imageIDs[firstImage.ID] = struct{}{}

			Expect(firstImage.Buildpacks).To(HaveLen(7))

			Expect(firstImage.Buildpacks[2].Key).To(Equal(buildpackInfo.Buildpack.ID))
			Expect(firstImage.Buildpacks[2].Layers).To(HaveKey("composer-packages"))

			Expect(logs.String()).To(ContainSubstring("Running 'composer install --no-progress --no-dev'"))

			// Second pack build
			Expect(fs.Copy(filepath.Join("testdata", "app_with_no_deps", "composer.json"), filepath.Join(source, "composer.json"))).To(Succeed())
			Expect(fs.Copy(filepath.Join("testdata", "app_with_no_deps", "composer.lock"), filepath.Join(source, "composer.lock"))).To(Succeed())

			secondImage, logs, err = build.
				Execute(name, source)
			Expect(err).NotTo(HaveOccurred())

			imageIDs[secondImage.ID] = struct{}{}

			Expect(secondImage.Buildpacks).To(HaveLen(7))

			Expect(secondImage.Buildpacks[2].Key).To(Equal(buildpackInfo.Buildpack.ID))
			Expect(secondImage.Buildpacks[2].Layers).To(HaveKey("composer-packages"))

			Expect(logs.String()).To(ContainSubstring("Running 'composer install --no-progress --no-dev'"))
			Expect(logs.String()).NotTo(ContainSubstring(fmt.Sprintf("Reusing cached layer /layers/%s/composer-packages", strings.ReplaceAll(buildpackInfo.Buildpack.ID, "/", "_"))))

			Expect(secondImage.Buildpacks[2].Layers["composer-packages"].SHA).NotTo(Equal(firstImage.Buildpacks[2].Layers["composer-packages"].SHA))
		})
	})

	context("when a vendored app is rebuilt and composer.lock does not change", func() {
		it("reuses the vendor layer from a previous build", func() {
			var (
				err         error
				logs        fmt.Stringer
				firstImage  occam.Image
				secondImage occam.Image
				thirdImage  occam.Image
			)

			source, err = occam.Source(filepath.Join("testdata", "with_vendored_packages"))
			Expect(err).NotTo(HaveOccurred())

			build := pack.WithNoColor().Build.
				WithPullPolicy("never").
				WithBuildpacks(buildpacksArray...).
				WithEnv(map[string]string{
					"BP_PHP_SERVER": "nginx",
				})

			firstImage, logs, err = build.Execute(name, source)
			Expect(err).NotTo(HaveOccurred())

			imageIDs[firstImage.ID] = struct{}{}

			Expect(firstImage.Buildpacks).To(HaveLen(7))

			Expect(firstImage.Buildpacks[2].Key).To(Equal(buildpackInfo.Buildpack.ID))
			Expect(firstImage.Buildpacks[2].Layers).To(HaveKey("composer-packages"))

			Expect(logs.String()).To(ContainSubstring("Running 'composer install --no-progress --no-dev'"))

			// Second pack build with BP_RUN_COMPOSER_INSTALL set to false
			context("with BP_RUN_COMPOSER_INSTALL set to false", func() {
				it.Before(func() {
					Expect(os.Setenv("BP_RUN_COMPOSER_INSTALL", "false")).To(Succeed())
				})
				it.After(func() {
					Expect(os.Unsetenv("BP_RUN_COMPOSER_INSTALL")).To(Succeed())
				})
				secondImage, logs, err = build.Execute(name, source)
				Expect(err).NotTo(HaveOccurred())

				imageIDs[secondImage.ID] = struct{}{}

				Expect(secondImage.Buildpacks).To(HaveLen(7))

				Expect(secondImage.Buildpacks[2].Key).To(Equal(buildpackInfo.Buildpack.ID))
				Expect(secondImage.Buildpacks[2].Layers).To(HaveKey("composer-packages"))

				it("does not run composer install again", func() {
					Expect(logs.String()).NotTo(ContainSubstring("Running 'composer install --no-progress --no-dev'"))
				})
				Expect(logs.String()).To(ContainSubstring("Detected existing vendored packages, replacing with cached vendored packages"))
				Expect(logs.String()).To(ContainSubstring(fmt.Sprintf("Reusing cached layer /layers/%s/composer-packages", strings.ReplaceAll(buildpackInfo.Buildpack.ID, "/", "_"))))

				Expect(secondImage.Buildpacks[2].Layers["composer-packages"].SHA).To(Equal(firstImage.Buildpacks[2].Layers["composer-packages"].SHA))
			})

			// Third pack build with BP_RUN_COMPOSER_INSTALL set to true
			context("with BP_RUN_COMPOSER_INSTALL set to true", func() {
				it.Before(func() {
					Expect(os.Setenv("BP_RUN_COMPOSER_INSTALL", "true")).To(Succeed())
				})
				it.After(func() {
					Expect(os.Unsetenv("BP_RUN_COMPOSER_INSTALL")).To(Succeed())
				})
				thirdImage, logs, err = build.Execute(name, source)
				Expect(err).NotTo(HaveOccurred())

				imageIDs[thirdImage.ID] = struct{}{}

				Expect(thirdImage.Buildpacks).To(HaveLen(7))

				Expect(thirdImage.Buildpacks[2].Key).To(Equal(buildpackInfo.Buildpack.ID))
				Expect(thirdImage.Buildpacks[2].Layers).To(HaveKey("composer-packages"))

				it("does run composer install again", func() {
					Expect(logs.String()).To(ContainSubstring("Running 'composer install --no-progress --no-dev'"))
				})
				Expect(logs.String()).To(ContainSubstring("Detected existing vendored packages, replacing with cached vendored packages"))
				Expect(logs.String()).To(ContainSubstring(fmt.Sprintf("Reusing cached layer /layers/%s/composer-packages", strings.ReplaceAll(buildpackInfo.Buildpack.ID, "/", "_"))))

				Expect(thirdImage.Buildpacks[2].Layers["composer-packages"].SHA).To(Equal(firstImage.Buildpacks[2].Layers["composer-packages"].SHA))
			})
		})
	})
}
