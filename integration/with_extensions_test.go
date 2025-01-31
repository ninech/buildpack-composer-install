package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/paketo-buildpacks/occam"
	"github.com/sclevine/spec"

	. "github.com/onsi/gomega"
	. "github.com/paketo-buildpacks/occam/matchers"
)

func testWithExtensions(t *testing.T, context spec.G, it spec.S) {
	var (
		Expect     = NewWithT(t).Expect
		Eventually = NewWithT(t).Eventually

		pack   occam.Pack
		docker occam.Docker
	)

	it.Before(func() {
		pack = occam.NewPack().WithVerbose().WithNoColor()
		docker = occam.NewDocker()
	})

	context("with extensions", func() {
		var (
			image     occam.Image
			container occam.Container

			name   string
			source string
		)

		it.Before(func() {
			var err error
			name, err = occam.RandomName()
			Expect(err).NotTo(HaveOccurred())
		})

		it.After(func() {
			Expect(docker.Container.Remove.Execute(container.ID)).To(Succeed())
			Expect(docker.Image.Remove.Execute(image.ID)).To(Succeed())
			Expect(docker.Volume.Remove.Execute(occam.CacheVolumeNames(name))).To(Succeed())
			Expect(os.RemoveAll(source)).To(Succeed())
		})

		it("launches PHP with extensions", func() {
			var err error
			var logs fmt.Stringer

			source, err = occam.Source(filepath.Join("testdata", "with_extensions"))
			Expect(err).NotTo(HaveOccurred())

			image, logs, err = pack.Build.
				WithPullPolicy("never").
				WithBuildpacks(buildpacksArray...).
				WithEnv(map[string]string{
					"BP_LOG_LEVEL":  "DEBUG",
					"BP_PHP_SERVER": "nginx",
				}).
				Execute(name, source)
			Expect(err).ToNot(HaveOccurred(), logs.String)

			Expect(logs).To(ContainSubstring("Running 'composer check-platform-reqs'"))
			Expect(logs).To(ContainSubstring("Found extensions 'openssl, fileinfo, gd, mysqli, zip'"))

			container, err = docker.Container.Run.
				WithEnv(map[string]string{"PORT": "8765"}).
				WithPublish("8765").
				Execute(image.ID)
			Expect(err).NotTo(HaveOccurred())

			// Note that `mbstring` is not included, since it is not available in `php-dist` for unknown reasons
			extensionsMatcher := And(
				ContainSubstring("zip"),
				ContainSubstring("openssl"),
				ContainSubstring("gd"),
				ContainSubstring("fileinfo"),
				ContainSubstring("mysqli"),
			)

			Eventually(container).Should(Serve(extensionsMatcher).OnPort(8765).WithEndpoint("/extensions.php"))
		})
	})
}
