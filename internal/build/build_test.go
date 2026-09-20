package build

import (
	"runtime"
	"strings"
	"testing"
)

// The platform is the release's name for the target, so it has to agree with what the
// runtime says this binary is — anything else would send a Pi owner to the wrong download.
func TestPlatformNamesThisBinarysTarget(t *testing.T) {
	got := Platform()
	if !strings.HasPrefix(got, runtime.GOOS+"/") {
		t.Fatalf("Platform() = %q, want it to start with %q", got, runtime.GOOS+"/")
	}
	if runtime.GOARCH != "arm" && got != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("Platform() = %q, want %q", got, runtime.GOOS+"/"+runtime.GOARCH)
	}
	if runtime.GOARCH == "arm" && !strings.HasPrefix(got, runtime.GOOS+"/armv") {
		t.Errorf("Platform() = %q on 32-bit ARM, want the ARM level named (linux/armv7)", got)
	}
}

func TestStringCarriesTheVersionAndThePlatform(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	Version = "v9.8.7"
	if got, want := String(), "briard-tether v9.8.7 "+Platform(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
