package watch

import (
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestRequiresCompDB(t *testing.T) {
	cases := []struct {
		path string
		op   fsnotify.Op
		want bool
	}{
		{`Source/Foo.cpp`, fsnotify.Write, false},
		{`Source/Foo.cpp`, fsnotify.Create, true},
		{`Source/Foo.h`, fsnotify.Remove, true},
		{`Source/Game.Build.cs`, fsnotify.Write, true},
		{`Source/Game.Target.cs`, fsnotify.Write, true},
		{`Game.uproject`, fsnotify.Write, true},
		{`Plugins/Foo/Foo.uplugin`, fsnotify.Write, true},
		{`Config/DefaultEngine.ini`, fsnotify.Write, false},
	}
	for _, tc := range cases {
		if got := RequiresCompDB(tc.path, tc.op); got != tc.want {
			t.Errorf("RequiresCompDB(%q, %v) = %v, want %v", tc.path, tc.op, got, tc.want)
		}
	}
}

func TestIgnoredDirectory(t *testing.T) {
	for _, path := range []string{"Intermediate/Foo", "Saved/Logs", "Binaries/Win64", "DerivedDataCache/X"} {
		if !IgnoredDirectory(path) {
			t.Errorf("%q should be ignored", path)
		}
	}
	if IgnoredDirectory("Source/Runtime") {
		t.Fatal("Source must be watched")
	}
}
