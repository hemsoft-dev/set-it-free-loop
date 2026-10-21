//go:build linux

package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type gitRetirementCrashInput struct {
	UnitRoot, Stub, StubHash, ArchiveRoot string
	Intent                                gitRetirement
}

func TestGitFactoryRetirementCrashChild(t *testing.T) {
	raw := os.Getenv("SFL_GIT_RETIREMENT_CRASH")
	if raw == "" {
		return
	}
	var input gitRetirementCrashInput
	if json.Unmarshal([]byte(raw), &input) != nil {
		os.Exit(2)
	}
	manager := &fakeContainmentManager{owner: input.Intent.Unit.ManagerOwner}
	unit, err := newGitUnitOwner(context.Background(), input.UnitRoot, input.Stub, input.StubHash, manager, &fakeContainmentKernel{})
	if err != nil {
		os.Exit(3)
	}
	archive, err := openGitExportArchive(context.Background(), input.ArchiveRoot)
	if err != nil {
		os.Exit(4)
	}
	attempt := input.Intent.Unit.Binding.Attempt
	if moveGitRetirementDirectory(unit.root, archive.root, gitUnitDirectory(attempt), gitRetirementTrash("unit", attempt), input.Intent.UnitDirectory) != nil {
		os.Exit(5)
	}
	if _, err := os.Stdout.Write([]byte("renamed\n")); err != nil {
		os.Exit(6)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestGitFactoryRetirementRecoversAfterActualOwnerCrash(t *testing.T) {
	r, f, intent := gitRetirementFixture(t)
	input, _ := json.Marshal(gitRetirementCrashInput{f.root, f.stub, f.stubHash, r.archive.directory, intent})
	if f.owner.Close() != nil || r.archive.Close(context.Background()) != nil {
		t.Fatal("old owners close")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestGitFactoryRetirementCrashChild$")
	child.Env = []string{"SFL_GIT_RETIREMENT_CRASH=" + string(input)}
	output, err := child.StdoutPipe()
	if err != nil || child.Start() != nil {
		t.Fatal("crash owner start", err)
	}
	waited := false
	defer func() {
		if !waited {
			child.Process.Kill()
			child.Wait()
		}
	}()
	ready := make(chan error, 1)
	go func() {
		data := make([]byte, len("renamed\n"))
		_, err := io.ReadFull(output, data)
		if err == nil && string(data) != "renamed\n" {
			err = fmt.Errorf("unexpected fixture readiness")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal("crash boundary", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("crash owner did not reach durable rename")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	contender, err := newGitUnitOwner(ctx, f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	cancel()
	if err == nil || contender != nil {
		t.Fatal("crash owner did not retain unit lock")
	}
	if child.Process.Kill() != nil || child.Wait() == nil {
		t.Fatal("owner was not killed")
	}
	waited = true
	unit, err := newGitUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
	if err != nil {
		t.Fatal("post-crash unit open", err)
	}
	defer unit.Close()
	archive, err := openGitExportArchive(context.Background(), r.archive.directory)
	if err != nil {
		t.Fatal("post-crash archive open", err)
	}
	defer archive.Close(context.Background())
	if resumeGitRetirements(context.Background(), unit, archive) != nil {
		t.Fatal("post-crash retirement")
	}
	units, unitErr := unit.records()
	exports, exportErr := archive.records()
	intents, intentErr := gitRetirementIntents(archive)
	if unitErr != nil || exportErr != nil || intentErr != nil || len(units) != 0 || len(exports) != 0 || len(intents) != 0 {
		t.Fatal("post-crash records survived", unitErr, exportErr, intentErr)
	}
}

func TestGitFactoryRetiresMoreThanLifetimeRecordLimit(t *testing.T) {
	r, f, runtime := gitFactoryAssetsFixture(t)
	credentials, _, _, _, signs := credentialOwnerFixture(t)
	helper := gitFactoryTestHelper(t, r)
	if r.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("prepared fixture stop")
	}
	factory, err := newGitSourceFactory(context.Background(), credentials, f.owner, r.archive, runtime, helper, r.manifest, testJob().Release, r.binding.ReleaseHash, gitFactoryWorkRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer factory.Close(context.Background())
	store, _ := openFixtureStore(t)
	for i := 0; i < 140; i++ {
		if err := factory.PruneSource(context.Background(), store, controlTime.Add(time.Hour)); err != nil {
			t.Fatal("retirement", i, err)
		}
		binding := r.binding
		binding.Attempt = fmt.Sprintf("%032x", i+1)
		if err := f.owner.prepare(context.Background(), binding); err != nil {
			t.Fatal("lifetime limit still consumes slots", i, err)
		}
		if f.owner.stop(context.Background(), binding) != nil {
			t.Fatal("prepared attempt stop", i)
		}
	}
	if factory.PruneSource(context.Background(), store, controlTime.Add(time.Hour)) != nil {
		t.Fatal("final retirement")
	}
	records, err := f.owner.records()
	exports, exportErr := r.archive.records()
	intents, intentErr := gitRetirementIntents(r.archive)
	if err != nil || exportErr != nil || intentErr != nil || len(records) != 0 || len(exports) != 0 || len(intents) != 0 || signs.Load() != 0 || f.manager.starts != 0 {
		t.Fatal("retirement leaked records or dispatched work", len(records), len(exports), len(intents), err, exportErr, intentErr)
	}
}

// Duplicate the measured helper before disposing the fixture run's file set.
func gitFactoryTestHelper(t *testing.T, r *gitFactoryRun) *os.File {
	t.Helper()
	file, err := duplicateGitControlFile(r.files[2])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

func gitRetirementFixture(t *testing.T) (*gitFactoryRun, gitUnitFixture, gitRetirement) {
	t.Helper()
	_, binding, objects := gitMaterializationFixture(t, "sha1")
	r, f, _ := gitFactoryAssetsFixture(t, binding.Snapshot)
	if r.stop(context.Background()) != nil || r.close() != nil {
		t.Fatal("prepared fixture stop")
	}
	var wire bytes.Buffer
	if writeGitMaterialization(context.Background(), &wire, objects, r.binding, DefaultGitAcquisitionBounds()) != nil {
		t.Fatal("synthetic export")
	}
	m, err := r.archive.stage(context.Background(), r.binding, io.NopCloser(bytes.NewReader(wire.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	if m.Close() != nil {
		t.Fatal("synthetic materialization close")
	}
	intent, err := prepareGitRetirement(f.owner, r.archive, r.binding)
	if err != nil {
		t.Fatal("prepare retirement", err)
	}
	raw, _ := json.Marshal(intent)
	if writeAuth(r.archive.root, gitRetirementName(r.binding.Attempt), raw) != nil {
		t.Fatal("write retirement intent")
	}
	return r, f, intent
}

func TestGitFactoryRetirementResumesEveryDeletionBoundary(t *testing.T) {
	for _, phase := range []string{"intent", "unit-renamed", "export-renamed", "payload-removed", "unit-record-removed", "directories-removed"} {
		t.Run(phase, func(t *testing.T) {
			r, f, intent := gitRetirementFixture(t)
			attempt := r.binding.Attempt
			unitTrash, exportTrash := gitRetirementTrash("unit", attempt), gitRetirementTrash("export", attempt)
			if phase != "intent" {
				if moveGitRetirementDirectory(f.owner.root, r.archive.root, gitUnitDirectory(attempt), unitTrash, intent.UnitDirectory) != nil {
					t.Fatal("unit rename")
				}
			}
			if phase != "intent" && phase != "unit-renamed" {
				if moveGitRetirementDirectory(r.archive.root, r.archive.root, gitExportDirectory(attempt), exportTrash, intent.ExportDirectory) != nil {
					t.Fatal("export rename")
				}
			}
			if phase == "payload-removed" {
				if r.archive.root.Remove(exportTrash+"/payload.bin") != nil {
					t.Fatal("partial payload deletion")
				}
			}
			if phase == "unit-record-removed" {
				if r.archive.root.Remove(unitTrash+"/git-unit.json") != nil {
					t.Fatal("partial unit receipt deletion")
				}
			}
			if phase == "directories-removed" {
				if removeGitRetirementDirectory(context.Background(), r.archive, exportTrash, intent.ExportDirectory, intent.ExportFiles) != nil || removeGitRetirementDirectory(context.Background(), r.archive, unitTrash, intent.UnitDirectory, intent.UnitFiles) != nil {
					t.Fatal("directory deletion")
				}
			}
			if f.owner.Close() != nil || r.archive.Close(context.Background()) != nil {
				t.Fatal("old owners close")
			}
			unit, err := newGitUnitOwner(context.Background(), f.root, f.stub, f.stubHash, f.manager, f.manager.kernel)
			if err != nil {
				t.Fatal("unit reopen after interrupted deletion", err)
			}
			defer unit.Close()
			archive, err := openGitExportArchive(context.Background(), r.archive.directory)
			if err != nil {
				t.Fatal("archive reopen after interrupted deletion", err)
			}
			defer archive.Close(context.Background())
			if err := resumeGitRetirements(context.Background(), unit, archive); err != nil {
				t.Fatal("resume deletion", phase, err)
			}
			units, unitErr := unit.records()
			exports, exportErr := archive.records()
			intents, intentErr := gitRetirementIntents(archive)
			if unitErr != nil || exportErr != nil || intentErr != nil || len(units) != 0 || len(exports) != 0 || len(intents) != 0 {
				t.Fatal("partial retirement survived", phase, unitErr, exportErr, intentErr)
			}
		})
	}
}

func TestGitFactoryRetirementRefusesForeignReplacementAndCancellation(t *testing.T) {
	for _, kind := range []string{"new-file", "replacement-file", "replacement-directory", "changed-receipt", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			r, f, intent := gitRetirementFixture(t)
			attempt := r.binding.Attempt
			trash := gitRetirementTrash("unit", attempt)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind != "canceled" {
				if moveGitRetirementDirectory(f.owner.root, r.archive.root, gitUnitDirectory(attempt), trash, intent.UnitDirectory) != nil {
					t.Fatal("unit rename")
				}
			}
			foreign := ""
			switch kind {
			case "new-file":
				foreign = trash + "/foreign"
				if r.archive.root.WriteFile(foreign, []byte("preserve"), 0600) != nil {
					t.Fatal("foreign file")
				}
			case "replacement-file":
				foreign = trash + "/git-unit.json"
				raw, err := r.archive.root.ReadFile(foreign)
				if err != nil {
					t.Fatal(err)
				}
				if r.archive.root.Rename(foreign, trash+"/old.json") != nil || r.archive.root.WriteFile(foreign, raw, 0600) != nil || r.archive.root.Remove(trash+"/old.json") != nil {
					t.Fatal("replace receipt inode")
				}
			case "replacement-directory":
				if r.archive.root.Rename(trash, trash+"-held") != nil || r.archive.root.Mkdir(trash, 0700) != nil {
					t.Fatal("replace directory inode")
				}
			case "changed-receipt":
				intent.Unit.StubHash = strings.Repeat("e", 64)
			case "canceled":
				cancel()
			}
			if finishGitRetirement(ctx, f.owner, r.archive, intent) == nil {
				t.Fatal("uncertain retirement completed")
			}
			if _, err := r.archive.root.Lstat(gitRetirementName(attempt)); err != nil {
				t.Fatal("lost recovery intent", err)
			}
			if foreign != "" {
				if _, err := r.archive.root.Lstat(foreign); err != nil {
					t.Fatal("foreign replacement deleted", err)
				}
			}
			if _, err := r.archive.root.Lstat(gitExportDirectory(attempt) + "/payload.bin"); err != nil {
				t.Fatal("foreign unit evidence caused the other journal's payload to be deleted", err)
			}
		})
	}
}
