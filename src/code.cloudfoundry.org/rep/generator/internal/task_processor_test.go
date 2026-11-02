package internal_test

import (
	"errors"

	"code.cloudfoundry.org/bbs/fake_bbs"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/bbs/models/test/model_helpers"
	fakeecrhelper "code.cloudfoundry.org/ecrhelper/fakes"
	"code.cloudfoundry.org/executor"
	"code.cloudfoundry.org/lager/v3/lagertest"
	"code.cloudfoundry.org/rep"
	"code.cloudfoundry.org/rep/generator/internal"
	"code.cloudfoundry.org/rep/generator/internal/fake_internal"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var processor internal.TaskProcessor

var _ = Describe("TaskProcessor", func() {
	var (
		bbsClient                *fake_bbs.FakeInternalClient
		expectedCellID, taskGuid string
		containerDelegate        *fake_internal.FakeContainerDelegate
		logger                   *lagertest.TestLogger
		task                     *models.Task
		expectedRunRequest       executor.RunRequest
		container                executor.Container
	)

	BeforeEach(func() {
		var err error

		bbsClient = &fake_bbs.FakeInternalClient{}
		containerDelegate = &fake_internal.FakeContainerDelegate{}
		logger = lagertest.NewTestLogger("task-processor")

		expectedCellID = "the-cell"
		taskGuid = "the-guid"

		processor = internal.NewTaskProcessor(bbsClient, containerDelegate, expectedCellID, rep.StackPathMap{}, "", nil)

		task = model_helpers.NewValidTask(taskGuid)
		runRequestConversionHelper := rep.RunRequestConversionHelper{ECRHelper: &fakeecrhelper.FakeECRHelper{}}
		expectedRunRequest, err = runRequestConversionHelper.NewRunRequestFromTask(task, rep.StackPathMap{}, "")
		Expect(err).NotTo(HaveOccurred())

		bbsClient.TaskByGuidReturns(task, nil)
	})

	itProcessesAnActiveContainer := func() {
		BeforeEach(func() {
			bbsClient.StartTaskReturns(true, nil)
		})

		JustBeforeEach(func() {
			processor.Process(logger, "some-trace-id", container)
		})

		It("starts the task", func() {
			Expect(bbsClient.StartTaskCallCount()).To(Equal(1))
			_, traceID, guid, cellID := bbsClient.StartTaskArgsForCall(0)
			Expect(traceID).To(Equal("some-trace-id"))
			Expect(guid).To(Equal(taskGuid))
			Expect(cellID).To(Equal(expectedCellID))
		})

		It("runs the container", func() {
			Expect(containerDelegate.RunContainerCallCount()).To(Equal(1))
			_, traceID, runReq := containerDelegate.RunContainerArgsForCall(0)
			Expect(traceID).To(Equal("some-trace-id"))
			Expect(runReq).To(Equal(&expectedRunRequest))
		})

		Context("when the task hasn't changed", func() {
			BeforeEach(func() {
				bbsClient.StartTaskReturns(false, nil)
			})

			It("does not run the container", func() {
				Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
			})
		})

		Context("when fetching the task fails", func() {
			BeforeEach(func() {
				bbsClient.TaskByGuidReturns(nil, errors.New("boom"))
			})

			It("does not run the container", func() {
				Expect(bbsClient.StartTaskCallCount()).To(Equal(1))
				Expect(bbsClient.TaskByGuidCallCount()).To(Equal(1))
				Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
				Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(0))
			})
		})

		Context("when creating the run request fails", func() {
			BeforeEach(func() {
				task.RootFs = "% s"
			})

			It("does not run the container", func() {
				Expect(bbsClient.StartTaskCallCount()).To(Equal(1))
				Expect(bbsClient.TaskByGuidCallCount()).To(Equal(1))
				Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
				Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(0))
			})
		})

		Context("when the task selects a VM backend with a rootfs that is not preloaded", func() {
			BeforeEach(func() {
				task.RootFs = "docker:///cloudfoundry/test"
				task.EnvironmentVariables = append(task.EnvironmentVariables,
					&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "vm-qemu"})
			})

			It("does not run the container", func() {
				Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
			})

			It("completes the task with failure and the conversion error as the reason", func() {
				Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
				_, traceID, guid, cellId, failed, reason, result := bbsClient.CompleteTaskArgsForCall(0)
				Expect(traceID).To(Equal("some-trace-id"))
				Expect(guid).To(Equal(taskGuid))
				Expect(cellId).To(Equal(expectedCellID))
				Expect(failed).To(BeTrue())
				Expect(reason).To(ContainSubstring(rep.ErrVMBackendRootFS.Error()))
				Expect(reason).To(ContainSubstring(`backend "vm-qemu"`))
				Expect(result).To(BeEmpty())
			})

			It("deletes the reserved container", func() {
				Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
				_, traceID, guid := containerDelegate.DeleteContainerArgsForCall(0)
				Expect(traceID).To(Equal("some-trace-id"))
				Expect(guid).To(Equal(taskGuid))
			})

			Context("when completing the task fails", func() {
				BeforeEach(func() {
					bbsClient.CompleteTaskReturns(errors.New("boom"))
				})

				It("still deletes the reserved container", func() {
					Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
				})

				It("does not run the container", func() {
					Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
				})
			})
		})

		Context("when the task selects a gvisor backend with a docker rootfs", func() {
			BeforeEach(func() {
				task.RootFs = "docker:///cloudfoundry/test"
				task.EnvironmentVariables = append(task.EnvironmentVariables,
					&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "oci-gvisor"})
				containerDelegate.RunContainerReturns(true)
			})

			It("runs the container and does not complete the task", func() {
				Expect(containerDelegate.RunContainerCallCount()).To(Equal(1))
				Expect(bbsClient.CompleteTaskCallCount()).To(Equal(0))
			})
		})

		Context("when the cell requires an execution backend", func() {
			var policy rep.ExecutionBackendPolicy

			BeforeEach(func() {
				var err error
				policy, err = rep.NewExecutionBackendPolicy([]string{"oci-gvisor"}, []string{"oci-gvisor"}, []string{"cf-app-staging"})
				Expect(err).NotTo(HaveOccurred())
				processor = internal.NewTaskProcessor(bbsClient, containerDelegate, expectedCellID, rep.StackPathMap{}, "", &policy)
				containerDelegate.RunContainerReturns(true)
			})

			Context("and a task outside the exempt domains selects no backend", func() {
				BeforeEach(func() {
					task.Domain = "cf-tasks"
				})

				It("does not run the container", func() {
					Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
				})

				It("completes the task with failure and names what the cell requires", func() {
					Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
					_, _, guid, cellId, failed, reason, _ := bbsClient.CompleteTaskArgsForCall(0)
					Expect(guid).To(Equal(taskGuid))
					Expect(cellId).To(Equal(expectedCellID))
					Expect(failed).To(BeTrue())
					Expect(reason).To(ContainSubstring(rep.ErrExecutionBackendNotAccepted.Error()))
					Expect(reason).To(ContainSubstring(`this cell requires "oci-gvisor"`))
				})

				It("deletes the reserved container", func() {
					Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
				})
			})

			Context("and a task in an exempt domain selects no backend", func() {
				BeforeEach(func() {
					task.Domain = "cf-app-staging"
				})

				It("runs the container and does not complete the task", func() {
					Expect(containerDelegate.RunContainerCallCount()).To(Equal(1))
					Expect(bbsClient.CompleteTaskCallCount()).To(Equal(0))
				})
			})

			Context("and a task selects a backend that the cell does not offer", func() {
				BeforeEach(func() {
					task.Domain = "cf-app-staging"
					task.EnvironmentVariables = append(task.EnvironmentVariables,
						&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "vm-qemu"})
				})

				It("completes the task with failure even in an exempt domain", func() {
					Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
					Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
					_, _, _, _, failed, reason, _ := bbsClient.CompleteTaskArgsForCall(0)
					Expect(failed).To(BeTrue())
					Expect(reason).To(ContainSubstring(rep.ErrExecutionBackendNotOffered.Error()))
				})
			})
		})

		Context("when running the container fails", func() {
			BeforeEach(func() {
				containerDelegate.RunContainerReturns(false)
			})

			It("completes the task with failure", func() {
				Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
				_, traceID, guid, cellId, failed, reason, _ := bbsClient.CompleteTaskArgsForCall(0)
				Expect(traceID).To(Equal("some-trace-id"))
				Expect(guid).To(Equal(taskGuid))
				Expect(cellId).To(Equal(expectedCellID))
				Expect(failed).To(BeTrue())
				Expect(reason).To(Equal(internal.TaskCompletionReasonFailedToRunContainer))
			})
		})

		Context("when starting the task fails", func() {
			Context("because of an invalid state transition", func() {
				BeforeEach(func() {
					bbsClient.StartTaskReturns(false, models.NewTaskTransitionError(
						models.Task_Pending,
						models.Task_Running,
					))
				})

				It("deletes the container", func() {
					Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
					Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
					_, traceID, guid := containerDelegate.DeleteContainerArgsForCall(0)
					Expect(traceID).To(Equal("some-trace-id"))
					Expect(guid).To(Equal(taskGuid))
				})
			})

			Context("because a resource was not found", func() {
				BeforeEach(func() {
					bbsClient.StartTaskReturns(false, models.ErrResourceNotFound)
				})

				It("deletes the container", func() {
					Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
					Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
					_, traceID, guid := containerDelegate.DeleteContainerArgsForCall(0)
					Expect(traceID).To(Equal("some-trace-id"))
					Expect(guid).To(Equal(taskGuid))
				})
			})

			Context("for another reason", func() {
				BeforeEach(func() {
					bbsClient.StartTaskReturns(false, errors.New("boom"))
				})

				It("does not delete the container", func() {
					Expect(containerDelegate.RunContainerCallCount()).To(Equal(0))
					Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(0))
				})
			})
		})
	}

	Context("when the container is initializing", func() {
		BeforeEach(func() {
			container = executor.Container{
				State: executor.StateInitializing,
				Guid:  taskGuid,
			}
		})

		itProcessesAnActiveContainer()
	})

	Context("when the container is created", func() {
		BeforeEach(func() {
			container = executor.Container{
				State: executor.StateCreated,
				Guid:  taskGuid,
			}
		})

		itProcessesAnActiveContainer()
	})

	Context("when the container is running", func() {
		BeforeEach(func() {
			container = executor.Container{
				State: executor.StateRunning,
				Guid:  taskGuid,
			}
		})

		itProcessesAnActiveContainer()
	})

	Context("when the container is reserved", func() {
		BeforeEach(func() {
			container = executor.Container{
				State: executor.StateReserved,
				Guid:  taskGuid,
			}
		})

		itProcessesAnActiveContainer()
	})

	Context("when the container is completed", func() {
		BeforeEach(func() {
			container = executor.Container{
				State: executor.StateCompleted,
				Guid:  taskGuid,
				RunResult: executor.ContainerRunResult{
					Failed:        true,
					FailureReason: "oh nooooooooooooo mr bill",
				},
			}
		})

		JustBeforeEach(func() {
			processor.Process(logger, "some-trace-id", container)
		})

		It("deletes the container", func() {
			Expect(containerDelegate.DeleteContainerCallCount()).To(Equal(1))
			_, traceID, guid := containerDelegate.DeleteContainerArgsForCall(0)
			Expect(traceID).To(Equal("some-trace-id"))
			Expect(guid).To(Equal(taskGuid))
		})

		It("completes the task", func() {
			Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
			_, traceID, guid, cellID, failed, failureReason, result := bbsClient.CompleteTaskArgsForCall(0)
			Expect(traceID).To(Equal("some-trace-id"))
			Expect(guid).To(Equal(taskGuid))
			Expect(cellID).To(Equal(expectedCellID))
			Expect(failed).To(Equal(true))
			Expect(failureReason).To(Equal("oh nooooooooooooo mr bill"))
			Expect(result).To(Equal(""))
		})

		Context("when the task failed but is retryable", func() {
			BeforeEach(func() {
				container.RunResult.Retryable = true
				container.RunResult.FailureReason = "failed really bad!!"
			})

			It("rejects the task", func() {
				Expect(bbsClient.RejectTaskCallCount()).To(Equal(1))
				_, traceID, guid, reason := bbsClient.RejectTaskArgsForCall(0)
				Expect(traceID).To(Equal("some-trace-id"))
				Expect(guid).To(Equal(taskGuid))
				Expect(reason).To(Equal("failed really bad!!"))
			})
		})

		Context("when completing the task fails", func() {
			Context("because of an invalid state transition", func() {
				BeforeEach(func() {
					bbsClient.CompleteTaskReturns(models.NewTaskTransitionError(models.Task_Running, models.Task_Completed))
				})

				It("completes the task with failure", func() {
					Expect(bbsClient.CompleteTaskCallCount()).To(Equal(2))
					_, traceID, guid, cellID, failed, reason, result := bbsClient.CompleteTaskArgsForCall(1)
					Expect(traceID).To(Equal("some-trace-id"))
					Expect(guid).To(Equal(taskGuid))
					Expect(cellID).To(Equal(expectedCellID))
					Expect(failed).To(Equal(true))
					Expect(reason).To(Equal(internal.TaskCompletionReasonInvalidTransition))
					Expect(result).To(Equal(""))
				})
			})
		})

		Context("when the container run succeeds", func() {
			BeforeEach(func() {
				container.RunResult = executor.ContainerRunResult{
					Failed: false,
				}
				container.Tags = executor.Tags{
					rep.ResultFileTag: "foobar",
				}

				containerDelegate.FetchContainerResultFileReturns("i am a result yo", nil)
			})

			It("fetches the result file and completes the task", func() {
				Expect(containerDelegate.FetchContainerResultFileCallCount()).To(Equal(1))
				_, guid, tag := containerDelegate.FetchContainerResultFileArgsForCall(0)
				Expect(guid).To(Equal(taskGuid))
				Expect(tag).To(Equal(container.Tags[rep.ResultFileTag]))

				Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
				_, traceID, guid, cellID, failed, failureReason, result := bbsClient.CompleteTaskArgsForCall(0)
				Expect(traceID).To(Equal("some-trace-id"))
				Expect(guid).To(Equal(taskGuid))
				Expect(cellID).To(Equal(expectedCellID))
				Expect(failed).To(Equal(false))
				Expect(failureReason).To(Equal(""))
				Expect(result).To(Equal("i am a result yo"))
			})

			Context("and there is no result file tag", func() {
				BeforeEach(func() {
					container.Tags = executor.Tags{}
				})

				It("does not attempt to fetch the result file and completes the task", func() {
					Expect(containerDelegate.FetchContainerResultFileCallCount()).To(Equal(0))

					Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
					_, traceID, guid, cellID, failed, failureReason, result := bbsClient.CompleteTaskArgsForCall(0)
					Expect(traceID).To(Equal("some-trace-id"))
					Expect(guid).To(Equal(taskGuid))
					Expect(cellID).To(Equal(expectedCellID))
					Expect(failed).To(Equal(false))
					Expect(failureReason).To(Equal(""))
					Expect(result).To(Equal(""))
				})
			})

			Context("and fetching the container result fails", func() {
				BeforeEach(func() {
					containerDelegate.FetchContainerResultFileReturns("", errors.New("get outta here"))
				})

				It("completes the task with failure", func() {
					Expect(containerDelegate.FetchContainerResultFileCallCount()).To(Equal(1))
					Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
					_, traceID, guid, cellID, failed, reason, result := bbsClient.CompleteTaskArgsForCall(0)
					Expect(traceID).To(Equal("some-trace-id"))
					Expect(guid).To(Equal(taskGuid))
					Expect(cellID).To(Equal(expectedCellID))
					Expect(failed).To(Equal(true))
					Expect(reason).To(Equal(internal.TaskCompletionReasonFailedToFetchResult))
					Expect(result).To(Equal(""))
				})
			})
		})

		Context("when the container run fails with a result file", func() {
			BeforeEach(func() {
				container.RunResult = executor.ContainerRunResult{
					Failed:        true,
					FailureReason: "container failed for some reason",
				}
				container.Tags = executor.Tags{
					rep.ResultFileTag: "some-result-file",
				}
			})

			Context("and fetching the result file succeeds", func() {
				BeforeEach(func() {
					containerDelegate.FetchContainerResultFileReturns("the result before failure", nil)
				})

				It("completes the task with the failure reason and the result", func() {
					Expect(containerDelegate.FetchContainerResultFileCallCount()).To(Equal(1))
					Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
					_, traceID, guid, cellID, failed, reason, result := bbsClient.CompleteTaskArgsForCall(0)
					Expect(traceID).To(Equal("some-trace-id"))
					Expect(guid).To(Equal(taskGuid))
					Expect(cellID).To(Equal(expectedCellID))
					Expect(failed).To(Equal(true))
					Expect(reason).To(Equal("container failed for some reason"))
					Expect(result).To(Equal("the result before failure"))
				})
			})

			Context("and fetching the result file fails", func() {
				BeforeEach(func() {
					containerDelegate.FetchContainerResultFileReturns("", errors.New("some error"))
				})

				It("completes the task with the original failure reason", func() {
					Expect(containerDelegate.FetchContainerResultFileCallCount()).To(Equal(1))
					Expect(bbsClient.CompleteTaskCallCount()).To(Equal(1))
					_, traceID, guid, cellID, failed, reason, result := bbsClient.CompleteTaskArgsForCall(0)
					Expect(traceID).To(Equal("some-trace-id"))
					Expect(guid).To(Equal(taskGuid))
					Expect(cellID).To(Equal(expectedCellID))
					Expect(failed).To(Equal(true))
					Expect(reason).To(Equal("container failed for some reason"))
					Expect(result).To(Equal(""))
				})
			})
		})
	})
})
