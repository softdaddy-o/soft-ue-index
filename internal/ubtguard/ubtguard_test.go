package ubtguard

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBusy(t *testing.T) {
	cases := []struct {
		name string
		p    Process
		want bool
	}{
		{"ubt exe", Process{1, "UnrealBuildTool.exe", ""}, true},
		{"dotnet ubt", Process{2, "dotnet.exe", `dotnet UnrealBuildTool.dll ElpisEditor Win64`}, true},
		{"build.bat", Process{3, "cmd.exe", `cmd /c F:\Engine\Build\BatchFiles\Build.bat ElpisEditor`}, true},
		{"editor compile", Process{4, "UnrealEditor-Cmd.exe", `-run=x -Compile`}, true},
		{"plain editor", Process{5, "UnrealEditor.exe", `Elpis.uproject`}, false},
		{"unrelated dotnet", Process{6, "dotnet.exe", `dotnet foo.dll`}, false},
		{"own pid", Process{99, "dotnet.exe", `dotnet UnrealBuildTool.dll`}, false},
	}
	for _, c := range cases {
		if got := Busy([]Process{c.p}, 99) != ""; got != c.want {
			t.Errorf("%s: busy=%v want %v", c.name, got, c.want)
		}
	}
}

func TestWaitBacksOffThenProceeds(t *testing.T) {
	calls := 0
	list := func(context.Context) ([]Process, error) {
		calls++
		if calls < 4 {
			return []Process{{1, "UnrealBuildTool.exe", ""}}, nil
		}
		return nil, nil
	}
	var sleeps []time.Duration
	err := Wait(context.Background(), list, Options{Initial: time.Second, Max: 3 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	if len(sleeps) != 3 || sleeps[0] != want[0] || sleeps[1] != want[1] || sleeps[2] != want[2] {
		t.Fatalf("sleeps=%v want %v", sleeps, want)
	}
}

func TestWaitGivesUpAndHonoursContext(t *testing.T) {
	busy := func(context.Context) ([]Process, error) { return []Process{{1, "UnrealBuildTool.exe", ""}}, nil }
	err := Wait(context.Background(), busy, Options{Initial: time.Second, Total: 2 * time.Second,
		Sleep: func(context.Context, time.Duration) error { return nil }}, nil)
	var eb ErrBusy
	if !errors.As(err, &eb) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, busy, Options{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}

func TestListerErrorDoesNotBlock(t *testing.T) {
	list := func(context.Context) ([]Process, error) { return nil, errors.New("x") }
	if err := Wait(context.Background(), list, Options{}, nil); err != nil {
		t.Fatal(err)
	}
}
