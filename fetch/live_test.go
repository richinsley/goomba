package fetch

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richinsley/goomba/archive"
	"github.com/richinsley/goomba/record"
)

// Cases from rattler crates/rattler_package_streaming/tests/extract.rs:
// the conda_archives, tar_bz2_archives and url_archives templates (applied
// there to test_extract_conda, test_extract_tar_bz2, their async twins and
// test_extract_url_async). Each archive is fetched with its sha256, its md5
// is checked, and it is extracted.
var rattlerArchives = []struct{ url, sha256, md5 string }{
	// conda_archives
	{"https://conda.anaconda.org/conda-forge/win-64/conda-22.11.1-py38haa244fe_1.conda", "a8a44c5ff2b2f423546d49721ba2e3e632233c74a813c944adf8e5742834930e", "9987c96161034575f5a9c2be848960c5"},
	{"https://conda.anaconda.org/conda-forge/win-64/mamba-1.1.0-py39hb3d9227_2.conda", "c172acdf9cb7655dd224879b30361a657b09bb084b65f151e36a2b51e51a080a", "d87eb6aecfc0fe58299e6d6cfb252a7f"},
	{"https://conda.anaconda.org/conda-forge/noarch/mock-5.0.0-pyhd8ed1ab_0.conda", "8ef7378ae3bcac5f1db9d95291b5c5ef98464ce51c18f8ec902d9e2c7c1bc49b", "d9d75bfae9eab6df13d8cbe650b9762d"},
	{"https://conda.anaconda.org/conda-forge/linux-ppc64le/mujoco-2.3.1-ha3edaa6_0.conda", "007f27a98a150ac3fbbd5bdd708d35f807ba2e117a194f218b130890d461ce77", "910c94e2d1234e98196c4a64a82ff07e"},
	{"https://conda.anaconda.org/conda-forge/win-64/ruff-0.0.171-py310h298983d_0.conda", "25c755b97189ee066576b4ae3999d5e7ff4406d236b984742194e63941838dcd", "1ecacf57f20c0d1e4a04af0c8d4b54a3"},
	{"https://conda.anaconda.org/conda-forge/win-64/stir-5.0.2-py38h9224444_7.conda", "352fe747f7f09b09baa4b6561485b3f0d4271f6f798d34dae7116c3c9c6ba896", "7bb9eb9ddaaf4505777512c5ad2fc108"},
	// tar_bz2_archives
	{"https://conda.anaconda.org/conda-forge/win-64/conda-22.9.0-py38haa244fe_2.tar.bz2", "3c2c2e8e81bde5fb1ac4b014f51a62411feff004580c708c97a0ec2b7058cdc4", "36194591e28b9f2c107aa3d952ac4649"},
	{"https://conda.anaconda.org/conda-forge/win-64/mamba-1.0.0-py38hecfeebb_2.tar.bz2", "f44c4bc9c6916ecc0e33137431645b029ade22190c7144eead61446dcbcc6f97", "dede6252c964db3f3e41c7d30d07f6bf"},
	{"https://conda.anaconda.org/conda-forge/win-64/micromamba-1.1.0-0.tar.bz2", "5a1e1fe69a301e817cf2795ace03c9e4a42e97cd8984b6edbc8872dad00d5097", "3774689d66819fb50ff87fccefff6e88"},
	{"https://conda.anaconda.org/conda-forge/win-64/mock-2.0.0-py37_1000.tar.bz2", "e85695f074ce4f77715f8f4873cc02fa5150efe2e5dadf4c85292edd5ffb5163", "df844836b49b9bd0bc857e70783f221e"},
	{"https://conda.anaconda.org/conda-forge/noarch/pytweening-1.0.4-pyhd8ed1ab_0.tar.bz2", "81644bcb60d295f7923770b41daf2d90152ef54b9b094c26513be50fccd62125", "d5e0fafeaa727f0de1c81bfb6e0e63d8"},
	{"https://conda.anaconda.org/robostack/linux-64/ros-noetic-rosbridge-suite-0.11.14-py39h6fdeb60_14.tar.bz2", "4dd9893f1eee45e1579d1a4f5533ef67a84b5e4b7515de7ed0db1dd47adc6bc8", "47d2678d67ec7ebd49ade2b9943e597e"},
	{"https://conda.anaconda.org/conda-forge/win-64/zlib-1.2.8-vc10_0.tar.bz2", "ee9172dbe9ebd158e8e68d6d0f7dc2060f0c8230b44d2e9a3595b7cd7336b915", "8415564d07857a1069c0cd74e7eeb369"},
	// url_archives
	{"https://conda.anaconda.org/conda-forge/win-64/ruff-0.0.205-py39h5b3f8fb_0.conda", "8affd54b71aabc63ddc3944135a4b31462b09da7d1677a53cd31df50ffe07173", "bdfa0d81d2337eb713a66119754ad67a"},
	{"https://conda.anaconda.org/conda-forge/win-64/python-3.11.0-hcf16a7b_0_cpython.tar.bz2", "20d1f1b5dc620b745c325844545fd5c0cdbfdb2385a0e27ef1507399844c8c6d", "13ee3577afc291dabd2d9edc59736688"},
	{"https://conda.anaconda.org/conda-forge/linux-64/openmp-8.0.1-0.tar.bz2", "a3332e80c633be1ee20a41c7dd8810260a2132cf7d03f363d83752cad907bcfd", "b35241079152e5cc891c99368395b2c6"},
}

func TestLiveRattlerArchives(t *testing.T) {
	if os.Getenv("GOOMBA_LIVE") == "" {
		t.Skip("set GOOMBA_LIVE=1")
	}
	c := &Cache{Dir: t.TempDir()}
	for _, a := range rattlerArchives {
		fn := path.Base(a.url)
		t.Run(fn, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			p, err := c.Fetch(ctx, a.url, a.sha256, 0)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if sum := md5.Sum(data); hex.EncodeToString(sum[:]) != a.md5 {
				t.Fatalf("md5 %x, want %s", sum, a.md5)
			}
			dest := filepath.Join(t.TempDir(), "x")
			if err := archive.Extract(ctx, p, dest, archive.Limits{}); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dest, "info", "index.json"))
			if err != nil {
				t.Fatal(err)
			}
			var idx struct{ Name, Version, Build string }
			if err := json.Unmarshal(raw, &idx); err != nil {
				t.Fatal(err)
			}
			// read_package_file: the index names the archive.
			if !strings.HasPrefix(fn, idx.Name+"-"+idx.Version+"-"+idx.Build) {
				t.Fatalf("index.json names %s-%s-%s", idx.Name, idx.Version, idx.Build)
			}
		})
	}
}

// TestLiveExtracted fetches and extracts a real package over https with the
// default client.
func TestLiveExtracted(t *testing.T) {
	if os.Getenv("GOOMBA_LIVE") == "" {
		t.Skip("set GOOMBA_LIVE=1")
	}
	c := &Cache{Dir: t.TempDir()}
	rec := &record.PackageRecord{
		Name: "wheel", Version: "0.46.3", Build: "pyhd8ed1ab_0", Fn: wheelFn,
		SHA256: wheelSHA, Size: wheelSize,
		URL: "https://conda.anaconda.org/conda-forge/noarch/" + wheelFn,
	}
	p, err := c.Extracted(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p, "site-packages", "wheel", "__init__.py")); err != nil {
		t.Fatal(err)
	}
}
