package rep_test

import (
	"errors"

	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/bbs/models/test/model_helpers"
	fakeecrhelper "code.cloudfoundry.org/ecrhelper/fakes"
	"code.cloudfoundry.org/rep"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ExecutionBackendPolicy", func() {
	mustPolicy := func(offered, required, exempt []string) rep.ExecutionBackendPolicy {
		policy, err := rep.NewExecutionBackendPolicy(offered, required, exempt)
		Expect(err).NotTo(HaveOccurred())
		return policy
	}

	Describe("NewExecutionBackendPolicy", func() {
		It("accepts an empty configuration, which offers only oci-runc", func() {
			policy := mustPolicy(nil, nil, nil)
			Expect(policy.Advertised()).To(BeFalse())
			Expect(policy.PlacementTags()).To(BeEmpty())
			Expect(policy.Check("", "")).To(Succeed())
			Expect(policy.Check("oci-runc", "")).To(Succeed())
		})

		It("treats oci-runc in the offered list as always offered and does not advertise it", func() {
			policy := mustPolicy([]string{"oci-runc"}, nil, nil)
			Expect(policy.Advertised()).To(BeFalse())
			Expect(policy.PlacementTags()).To(BeEmpty())
		})

		It("refuses a required backend that the cell does not offer", func() {
			_, err := rep.NewExecutionBackendPolicy([]string{"vm-qemu"}, []string{"vm-firecracker"}, nil)
			Expect(err).To(MatchError(ContainSubstring(`required execution backend "vm-firecracker" is not offered`)))
		})

		It("allows oci-runc in the required list", func() {
			policy := mustPolicy([]string{"oci-gvisor"}, []string{"oci-runc", "oci-gvisor"}, nil)
			Expect(policy.Check("", "")).To(Succeed())
			Expect(policy.Check("oci-gvisor", "")).To(Succeed())
		})

		It("refuses an offered name that cannot travel in a placement tag", func() {
			_, err := rep.NewExecutionBackendPolicy([]string{"vm-qemu,vm-firecracker"}, nil, nil)
			Expect(err).To(MatchError(ContainSubstring("invalid offered execution backend")))

			_, err = rep.NewExecutionBackendPolicy([]string{""}, nil, nil)
			Expect(err).To(MatchError(ContainSubstring("invalid offered execution backend")))
		})

		It("refuses an empty exempt task domain", func() {
			_, err := rep.NewExecutionBackendPolicy([]string{"vm-qemu"}, []string{"vm-qemu"}, []string{""})
			Expect(err).To(MatchError(ContainSubstring("exempt task domain is empty")))
		})

		It("drops the exempt task domains when nothing is required", func() {
			policy := mustPolicy([]string{"vm-qemu"}, nil, []string{"cf-app-staging"})
			Expect(policy.PlacementTags()).To(Equal([]string{"execution-backend.offered:vm-qemu"}))
		})
	})

	Describe("placement tags", func() {
		It("advertises the policy in sorted, prefixed tags", func() {
			policy := mustPolicy(
				[]string{"vm-qemu", "oci-gvisor", "vm-firecracker"},
				[]string{"vm-qemu", "vm-firecracker"},
				[]string{"cf-app-staging"},
			)
			Expect(policy.Advertised()).To(BeTrue())
			Expect(policy.PlacementTags()).To(Equal([]string{
				"execution-backend.offered:oci-gvisor",
				"execution-backend.offered:vm-firecracker",
				"execution-backend.offered:vm-qemu",
				"execution-backend.required:vm-firecracker",
				"execution-backend.required:vm-qemu",
				"execution-backend.exempt-task-domain:cf-app-staging",
			}))
			for _, tag := range policy.PlacementTags() {
				Expect(rep.IsExecutionBackendPlacementTag(tag)).To(BeTrue())
			}
		})

		It("reads back the policy that the tags advertise, ignoring other tags", func() {
			policy := mustPolicy([]string{"vm-qemu"}, []string{"vm-qemu"}, []string{"cf-app-staging"})
			tags := append([]string{"isolation-segment-a", "chirp"}, policy.PlacementTags()...)

			read := rep.ExecutionBackendPolicyFromPlacementTags(tags)
			Expect(read.PlacementTags()).To(Equal(policy.PlacementTags()))
			Expect(read.Required()).To(Equal([]string{"vm-qemu"}))
			Expect(read.Check("vm-qemu", "")).To(Succeed())
			Expect(read.Check("", "cf-app-staging")).To(Succeed())
			Expect(read.Check("", "cf-tasks")).To(MatchError(rep.ErrExecutionBackendNotAccepted))
		})

		It("reads a cell without policy tags, such as one whose rep predates them, as offering only oci-runc", func() {
			read := rep.ExecutionBackendPolicyFromPlacementTags([]string{"chirp"})
			Expect(read.Advertised()).To(BeFalse())
			Expect(read.Required()).To(BeNil())
			Expect(read.Check("", "")).To(Succeed())
			Expect(read.Check("vm-qemu", "")).To(MatchError(rep.ErrExecutionBackendNotOffered))
		})

		It("does not treat other tags as reserved", func() {
			Expect(rep.IsExecutionBackendPlacementTag("execution-backends")).To(BeFalse())
			Expect(rep.IsExecutionBackendPlacementTag("isolation-segment-a")).To(BeFalse())
		})
	})

	Describe("Check", func() {
		var policy rep.ExecutionBackendPolicy

		Context("when the cell offers backends and requires none", func() {
			BeforeEach(func() {
				policy = mustPolicy([]string{"vm-qemu"}, nil, nil)
			})

			It("accepts the default and every offered backend", func() {
				Expect(policy.Check("", "")).To(Succeed())
				Expect(policy.Check("oci-runc", "")).To(Succeed())
				Expect(policy.Check("vm-qemu", "")).To(Succeed())
			})

			It("refuses a backend that it does not offer and names it", func() {
				err := policy.Check("vm-firecracker", "")
				Expect(err).To(MatchError(rep.ErrExecutionBackendNotOffered))
				Expect(err.Error()).To(Equal(`execution backend is not offered by this cell: "vm-firecracker"`))
				Expect(rep.IsExecutionBackendPolicyError(err)).To(BeTrue())
			})

			It("refuses a name that is not a backend", func() {
				Expect(policy.Check("vm-qmeu", "")).To(MatchError(rep.ErrExecutionBackendNotOffered))
			})
		})

		Context("when the cell requires backends", func() {
			BeforeEach(func() {
				policy = mustPolicy([]string{"vm-qemu", "vm-firecracker", "oci-gvisor"}, []string{"vm-qemu", "vm-firecracker"}, []string{"cf-app-staging"})
			})

			It("accepts a required backend for an LRP or a task", func() {
				Expect(policy.Check("vm-qemu", "")).To(Succeed())
				Expect(policy.Check("vm-firecracker", "cf-tasks")).To(Succeed())
			})

			It("refuses an LRP that selects no backend and names what the cell requires", func() {
				err := policy.Check("", "")
				Expect(err).To(MatchError(rep.ErrExecutionBackendNotAccepted))
				Expect(err.Error()).To(Equal(`execution backend is not accepted by this cell: "oci-runc", this cell requires "vm-firecracker" or "vm-qemu"`))
				Expect(rep.IsExecutionBackendPolicyError(err)).To(BeTrue())
			})

			It("refuses an offered backend that is not required", func() {
				Expect(policy.Check("oci-gvisor", "")).To(MatchError(rep.ErrExecutionBackendNotAccepted))
			})

			It("accepts any offered backend for a task in an exempt domain", func() {
				Expect(policy.Check("", "cf-app-staging")).To(Succeed())
				Expect(policy.Check("oci-gvisor", "cf-app-staging")).To(Succeed())
			})

			It("still refuses a backend it does not offer for a task in an exempt domain", func() {
				Expect(policy.Check("vm-qmeu", "cf-app-staging")).To(MatchError(rep.ErrExecutionBackendNotOffered))
			})

			It("does not apply the task exemption to an LRP, whose domain is not passed", func() {
				Expect(policy.Check("", "")).To(MatchError(rep.ErrExecutionBackendNotAccepted))
			})
		})

		It("does not classify other errors as policy errors", func() {
			Expect(rep.IsExecutionBackendPolicyError(errors.New("boom"))).To(BeFalse())
			Expect(rep.IsExecutionBackendPolicyError(rep.ErrVMBackendRootFS)).To(BeFalse())
		})
	})

	Describe("reading what a workload selects", func() {
		It("reads an LRP's backend from its launcher action, the same way the run request does", func() {
			desiredLRP := model_helpers.NewValidDesiredLRP("process-guid")
			desiredLRP.Action = models.WrapAction(&models.RunAction{
				Path: "/tmp/lifecycle/launcher",
				User: "vcap",
				Env: []*models.EnvironmentVariable{
					{Name: "CF_EXECUTION_BACKEND", Value: "vm-firecracker"},
					{Name: "CF_EXECUTION_BACKEND", Value: "vm-qemu"},
				},
			})
			desiredLRP.Sidecars = nil
			Expect(rep.ExecutionBackendForDesiredLRP(desiredLRP)).To(Equal("vm-qemu"))
		})

		It("reads an LRP without a selector as selecting none", func() {
			desiredLRP := model_helpers.NewValidDesiredLRP("process-guid")
			Expect(rep.ExecutionBackendForDesiredLRP(desiredLRP)).To(Equal(""))
		})

		It("reads a task's backend from its environment", func() {
			task := model_helpers.NewValidTask("task-guid")
			task.EnvironmentVariables = append(task.EnvironmentVariables, &models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "oci-gvisor"})
			Expect(rep.ExecutionBackendForTask(task)).To(Equal("oci-gvisor"))
		})

		It("reads a task without a definition as selecting none", func() {
			Expect(rep.ExecutionBackendForTask(&models.Task{TaskGuid: "task-guid"})).To(Equal(""))
			Expect(rep.ExecutionBackendForTask(nil)).To(Equal(""))
		})
	})

	Describe("the run request conversion", func() {
		var helper rep.RunRequestConversionHelper

		BeforeEach(func() {
			policy := mustPolicy([]string{"oci-gvisor"}, []string{"oci-gvisor"}, []string{"cf-app-staging"})
			helper = rep.RunRequestConversionHelper{ECRHelper: &fakeecrhelper.FakeECRHelper{}, ExecutionBackendPolicy: &policy}
		})

		It("refuses an LRP whose backend the cell does not accept", func() {
			desiredLRP := model_helpers.NewValidDesiredLRP("process-guid")
			lrpKey := models.NewActualLRPKey("process-guid", 0, "domain")
			instanceKey := models.NewActualLRPInstanceKey("instance-guid", "cell-id")
			_, err := helper.NewRunRequestFromDesiredLRP("container-guid", desiredLRP, &lrpKey, &instanceKey, rep.StackPathMap{}, "")
			Expect(err).To(MatchError(rep.ErrExecutionBackendNotAccepted))
		})

		It("builds the run request for an LRP whose backend the cell accepts", func() {
			desiredLRP := model_helpers.NewValidDesiredLRP("process-guid")
			desiredLRP.RootFs = "docker:///cloudfoundry/test"
			desiredLRP.Action = models.WrapAction(&models.RunAction{
				Path: "/tmp/lifecycle/launcher",
				User: "vcap",
				Env:  []*models.EnvironmentVariable{{Name: "CF_EXECUTION_BACKEND", Value: "oci-gvisor"}},
			})
			desiredLRP.Sidecars = nil
			lrpKey := models.NewActualLRPKey("process-guid", 0, "domain")
			instanceKey := models.NewActualLRPInstanceKey("instance-guid", "cell-id")
			runReq, err := helper.NewRunRequestFromDesiredLRP("container-guid", desiredLRP, &lrpKey, &instanceKey, rep.StackPathMap{}, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(runReq.ExecutionBackend).To(Equal("oci-gvisor"))
		})

		It("refuses a task whose backend the cell does not accept", func() {
			task := model_helpers.NewValidTask("task-guid")
			task.Domain = "cf-tasks"
			_, err := helper.NewRunRequestFromTask(task, rep.StackPathMap{}, "")
			Expect(err).To(MatchError(rep.ErrExecutionBackendNotAccepted))
		})

		It("builds the run request for a task in an exempt domain", func() {
			task := model_helpers.NewValidTask("task-guid")
			task.Domain = "cf-app-staging"
			_, err := helper.NewRunRequestFromTask(task, rep.StackPathMap{}, "")
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not check anything when the helper has no policy", func() {
			helper = rep.RunRequestConversionHelper{ECRHelper: &fakeecrhelper.FakeECRHelper{}}
			task := model_helpers.NewValidTask("task-guid")
			task.EnvironmentVariables = append(task.EnvironmentVariables, &models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "oci-gvisor"})
			_, err := helper.NewRunRequestFromTask(task, rep.StackPathMap{}, "")
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
