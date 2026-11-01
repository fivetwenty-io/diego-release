package rep_test

import (
	"encoding/json"
	"strings"

	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/bbs/models/test/model_helpers"
	fakeecrhelper "code.cloudfoundry.org/ecrhelper/fakes"
	"code.cloudfoundry.org/executor"
	"code.cloudfoundry.org/rep"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Folding sidecars for a VM backend", func() {
	var (
		helper       rep.RunRequestConversionHelper
		desiredLRP   *models.DesiredLRP
		stackPathMap rep.StackPathMap
		backend      string
	)

	env := func(pairs ...string) []*models.EnvironmentVariable {
		var out []*models.EnvironmentVariable
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, &models.EnvironmentVariable{Name: pairs[i], Value: pairs[i+1]})
		}
		return out
	}
	launcher := func(user, logSource string, args []string, envVars []*models.EnvironmentVariable) *models.Action {
		return models.WrapAction(&models.RunAction{
			Path:      "/tmp/lifecycle/launcher",
			Args:      args,
			User:      user,
			LogSource: logSource,
			Env:       envVars,
		})
	}
	appEnv := func(extra ...string) []*models.EnvironmentVariable {
		return env(append([]string{"VCAP_APPLICATION", "{}", "CF_EXECUTION_BACKEND", backend}, extra...)...)
	}
	ccAction := func(launchers ...*models.Action) *models.Action {
		actions := append([]*models.Action{}, launchers...)
		actions = append(actions, models.WrapAction(&models.RunAction{Path: "/tmp/lifecycle/diego-sshd", User: "vcap", LogSource: "CELL/SSHD"}))
		return models.WrapAction(&models.CodependentAction{Actions: actions})
	}
	sidecarOf := func(action *models.Action) *models.Sidecar {
		return &models.Sidecar{Action: action, MemoryMb: 64, DiskMb: 64}
	}
	convert := func() (executor.RunRequest, error) {
		actualLRP := model_helpers.NewValidActualLRP("the-process-guid", 2)
		return helper.NewRunRequestFromDesiredLRP("the-container-guid", desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
	}
	launcherCount := func(action *models.Action) int {
		count := 0
		var walk func(a *models.Action)
		walk = func(a *models.Action) {
			switch v := models.UnwrapAction(a).(type) {
			case *models.RunAction:
				if v.Path == "/tmp/lifecycle/launcher" {
					count++
				}
			case *models.CodependentAction:
				for _, child := range v.Actions {
					walk(child)
				}
			}
		}
		walk(action)
		return count
	}
	descriptionOf := func(run *models.RunAction) map[string]any {
		var value string
		for _, v := range run.Env {
			if v.Name == "CF_VM_SIDECARS" {
				value = v.Value
			}
		}
		Expect(value).NotTo(BeEmpty())
		var description map[string]any
		Expect(json.Unmarshal([]byte(value), &description)).To(Succeed())
		return description
	}

	BeforeEach(func() {
		helper = rep.RunRequestConversionHelper{ECRHelper: &fakeecrhelper.FakeECRHelper{}}
		stackPathMap = rep.StackPathMap{"cflinuxfs3": "cflinuxfs3:/var/vcap/packages/cflinuxfs3/rootfs.tar"}
		backend = "vm-qemu"

		desiredLRP = model_helpers.NewValidDesiredLRP("the-process-guid")
		desiredLRP.ImageLayers = nil
		desiredLRP.RootFs = "preloaded:cflinuxfs3"
		desiredLRP.EnvironmentVariables = env("LANG", "en_US.UTF-8")
		// Cloud Controller lists the app, then each sidecar, then the SSH
		// daemon in one codependent action and leaves the Sidecars field empty.
		desiredLRP.Action = ccAction(
			launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv("A", "1", "B", "2")),
			launcher("vcap", "APP/PROC/WEB/SIDECAR/ONE", []string{"app", "./one --port 8081", ""}, appEnv("A", "1", "B", "2")),
			launcher("vcap", "APP/PROC/WEB/SIDECAR/TWO", []string{"app", "./two", ""}, appEnv("A", "1", "B", "2")),
		)
		desiredLRP.Sidecars = nil
	})

	Context("when a vm-qemu LRP has two launcher sidecars", func() {
		It("adds the variable to the app's launcher and drops the sidecar launchers", func() {
			runReq, err := convert()
			Expect(err).NotTo(HaveOccurred())
			Expect(runReq.RunInfo.Sidecars).To(BeNil())

			codependent, ok := models.UnwrapAction(runReq.RunInfo.Action).(*models.CodependentAction)
			Expect(ok).To(BeTrue())
			Expect(codependent.Actions).To(HaveLen(2))

			app := models.UnwrapAction(codependent.Actions[0]).(*models.RunAction)
			Expect(app.Args).To(Equal([]string{"app", "./start", ""}))
			Expect(app.Env).To(HaveLen(len(appEnv("A", "1", "B", "2")) + 1))
			Expect(app.Env[len(app.Env)-1].Name).To(Equal("CF_VM_SIDECARS"))

			sshd := models.UnwrapAction(codependent.Actions[1]).(*models.RunAction)
			Expect(sshd.Path).To(Equal("/tmp/lifecycle/diego-sshd"))
		})

		It("encodes each sidecar's arguments and log source", func() {
			runReq, err := convert()
			Expect(err).NotTo(HaveOccurred())

			codependent := models.UnwrapAction(runReq.RunInfo.Action).(*models.CodependentAction)
			description := descriptionOf(models.UnwrapAction(codependent.Actions[0]).(*models.RunAction))
			Expect(description["version"]).To(BeEquivalentTo(1))
			Expect(description["sidecars"]).To(Equal([]any{
				map[string]any{
					"args":       []any{"app", "./one --port 8081", ""},
					"env":        []any{},
					"unset":      []any{},
					"log_source": "APP/PROC/WEB/SIDECAR/ONE",
				},
				map[string]any{
					"args":       []any{"app", "./two", ""},
					"env":        []any{},
					"unset":      []any{},
					"log_source": "APP/PROC/WEB/SIDECAR/TWO",
				},
			}))
		})

		It("leaves the desired LRP unchanged", func() {
			before := desiredLRP.Copy()
			_, err := convert()
			Expect(err).NotTo(HaveOccurred())
			Expect(desiredLRP).To(Equal(before))
			Expect(launcherCount(desiredLRP.Action)).To(Equal(3))
		})
	})

	Context("when a vm-firecracker LRP has a launcher sidecar", func() {
		BeforeEach(func() {
			backend = "vm-firecracker"
			desiredLRP.Action = ccAction(
				launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv()),
				launcher("vcap", "S", []string{"app", "./s", ""}, appEnv()),
			)
		})

		It("folds it", func() {
			runReq, err := convert()
			Expect(err).NotTo(HaveOccurred())
			Expect(runReq.RunInfo.ExecutionBackend).To(Equal("vm-firecracker"))
			Expect(runReq.RunInfo.Sidecars).To(BeNil())
			Expect(launcherCount(runReq.RunInfo.Action)).To(Equal(1))
		})
	})

	Context("when the sidecars are in the Sidecars field", func() {
		BeforeEach(func() {
			desiredLRP.Action = ccAction(launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv()))
			desiredLRP.Sidecars = []*models.Sidecar{sidecarOf(launcher("vcap", "FIELD", []string{"app", "./f", ""}, appEnv()))}
		})

		It("folds them after the ones in the action", func() {
			runReq, err := convert()
			Expect(err).NotTo(HaveOccurred())
			Expect(runReq.RunInfo.Sidecars).To(BeNil())

			codependent := models.UnwrapAction(runReq.RunInfo.Action).(*models.CodependentAction)
			sidecars := descriptionOf(models.UnwrapAction(codependent.Actions[0]).(*models.RunAction))["sidecars"].([]any)
			Expect(sidecars).To(HaveLen(1))
			Expect(sidecars[0].(map[string]any)["log_source"]).To(Equal("FIELD"))
		})
	})

	Context("when the app action is a single launcher and there are no sidecars", func() {
		BeforeEach(func() {
			desiredLRP.Action = launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv())
		})

		It("keeps the action and the sidecar list as they were", func() {
			runReq, err := convert()
			Expect(err).NotTo(HaveOccurred())
			Expect(runReq.RunInfo.Action).To(Equal(desiredLRP.Action))
			Expect(runReq.RunInfo.Sidecars).To(Equal([]executor.Sidecar{}))
		})
	})

	Context("when a sidecar's environment differs from the app's", func() {
		BeforeEach(func() {
			desiredLRP.Action = ccAction(
				launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv("A", "1", "B", "2")),
				launcher("vcap", "S", []string{"app", "./s", ""}, appEnv("Z", "zed", "A", "1", "A", "changed", "NEW", "value")),
			)
		})

		It("lists the changed and new variables in the sidecar's order, and the ones it lacks", func() {
			runReq, err := convert()
			Expect(err).NotTo(HaveOccurred())

			codependent := models.UnwrapAction(runReq.RunInfo.Action).(*models.CodependentAction)
			sidecar := descriptionOf(models.UnwrapAction(codependent.Actions[0]).(*models.RunAction))["sidecars"].([]any)[0].(map[string]any)
			Expect(sidecar["env"]).To(Equal([]any{"Z=zed", "A=changed", "NEW=value"}))
			Expect(sidecar["unset"]).To(Equal([]any{"B"}))
		})
	})

	Context("when a runC LRP has an extra process beside its sidecars", func() {
		BeforeEach(func() {
			backend = "oci-gvisor"
			desiredLRP.Action = models.WrapAction(models.Codependent(
				models.UnwrapAction(launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv())),
				models.UnwrapAction(launcher("vcap", "S", []string{"app", "./s", ""}, appEnv())),
				&models.RunAction{Path: "/usr/bin/something", User: "vcap"},
			))
		})

		It("leaves the action untouched", func() {
			runReq, err := convert()
			Expect(err).NotTo(HaveOccurred())
			Expect(runReq.RunInfo.Action).To(Equal(desiredLRP.Action))
		})
	})

	Context("when a runC LRP has sidecars", func() {
		BeforeEach(func() {
			backend = "oci-gvisor"
			desiredLRP.Action = ccAction(launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv()))
			desiredLRP.Sidecars = []*models.Sidecar{sidecarOf(launcher("root", "S", []string{"app", "./s", ""}, appEnv("X", "1")))}
		})

		It("passes the action and the sidecars through untouched", func() {
			runReq, err := convert()
			Expect(err).NotTo(HaveOccurred())
			Expect(runReq.RunInfo.Action).To(Equal(desiredLRP.Action))
			Expect(runReq.RunInfo.Sidecars).To(Equal([]executor.Sidecar{{Action: desiredLRP.Sidecars[0].Action, MemoryMB: 64, DiskMB: 64}}))
		})
	})

	DescribeTable("the refusals",
		func(arrange func(), reason string) {
			arrange()
			_, err := convert()
			Expect(err).To(MatchError(rep.ErrVMBackendSidecars))
			Expect(err.Error()).To(ContainSubstring(reason))
		},
		Entry("a sidecar launcher nested in a wrapper", func() {
			desiredLRP.Action = ccAction(
				launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv()),
				models.WrapAction(models.Serial(models.UnwrapAction(launcher("vcap", "S", []string{"app", "./s", ""}, appEnv())))),
			)
		}, "neither the launcher nor the SSH daemon"),
		Entry("a Sidecars field entry that is a wrapped action", func() {
			desiredLRP.Sidecars = []*models.Sidecar{sidecarOf(models.WrapAction(models.Serial(models.UnwrapAction(launcher("vcap", "S", []string{"app", "./s", ""}, appEnv())))))}
		}, "sidecar 3 is not a single run action"),
		Entry("a Sidecars field entry that is not the launcher", func() {
			desiredLRP.Sidecars = []*models.Sidecar{sidecarOf(models.WrapAction(&models.RunAction{Path: "/tmp/other", User: "vcap"}))}
		}, "sidecar 3 does not run through the launcher"),
		Entry("a Sidecars field entry without an action", func() {
			desiredLRP.Sidecars = []*models.Sidecar{{}}
		}, "sidecar 3 has no action"),
		Entry("an extra run that is neither the launcher nor the SSH daemon", func() {
			desiredLRP.Action = models.WrapAction(models.Codependent(
				models.UnwrapAction(launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv())),
				models.UnwrapAction(launcher("vcap", "S", []string{"app", "./s", ""}, appEnv())),
				&models.RunAction{Path: "/usr/bin/something", User: "vcap"},
			))
		}, "neither the launcher nor the SSH daemon"),
		Entry("a non-run child of the codependent action", func() {
			desiredLRP.Action = models.WrapAction(models.Codependent(
				models.UnwrapAction(launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv())),
				models.UnwrapAction(launcher("vcap", "S", []string{"app", "./s", ""}, appEnv())),
				models.Serial(&models.RunAction{Path: "/tmp/lifecycle/diego-sshd"}),
			))
		}, "neither the launcher nor the SSH daemon"),
		Entry("a sidecar with another user", func() {
			desiredLRP.Action = ccAction(
				launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv()),
				launcher("vcap", "ONE", []string{"app", "./one", ""}, appEnv()),
				launcher("root", "TWO", []string{"app", "./two", ""}, appEnv()),
			)
		}, `sidecar 2 runs as user "root", and the app runs as "vcap"`),
		Entry("nine sidecars", func() {
			launchers := []*models.Action{launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv())}
			for i := 0; i < 9; i++ {
				launchers = append(launchers, launcher("vcap", "S", []string{"app", "./s", ""}, appEnv()))
			}
			desiredLRP.Action = ccAction(launchers...)
		}, "the app has 9 sidecars, and at most 8 are allowed"),
		Entry("a description over 64 KiB", func() {
			desiredLRP.Action = ccAction(
				launcher("vcap", "APP/PROC/WEB", []string{"app", "./start", ""}, appEnv()),
				launcher("vcap", "S", []string{"app", strings.Repeat("x", 64*1024), ""}, appEnv()),
			)
		}, "the sidecar description is"),
		Entry("sidecars without a launcher in the app action", func() {
			desiredLRP.Action = ccAction()
			desiredLRP.EnvironmentVariables = appEnv()
			desiredLRP.Sidecars = []*models.Sidecar{sidecarOf(launcher("vcap", "S", []string{"app", "./s", ""}, appEnv()))}
		}, "the app action holds no launcher process"),
	)
})
