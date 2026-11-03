package rep_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/bbs/models/test/model_helpers"
	"code.cloudfoundry.org/bbs/test_helpers"
	fakeecrhelper "code.cloudfoundry.org/ecrhelper/fakes"
	"code.cloudfoundry.org/executor"
	"code.cloudfoundry.org/rep"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Resources", func() {
	Describe("ActualLRPKeyFromTags", func() {
		var (
			tags             executor.Tags
			lrpKey           *models.ActualLRPKey
			keyConversionErr error
		)

		BeforeEach(func() {
			tags = executor.Tags{
				rep.LifecycleTag:    rep.LRPLifecycle,
				rep.DomainTag:       "my-domain",
				rep.ProcessGuidTag:  "process-guid",
				rep.ProcessIndexTag: "999",
			}
		})

		JustBeforeEach(func() {
			lrpKey, keyConversionErr = rep.ActualLRPKeyFromTags(tags)
		})

		Context("when the tags are valid", func() {
			It("does not return an error", func() {
				Expect(keyConversionErr).NotTo(HaveOccurred())
			})

			It("converts a valid tags without error", func() {
				expectedKey := models.ActualLRPKey{
					ProcessGuid: "process-guid",
					Index:       999,
					Domain:      "my-domain",
				}
				Expect(*lrpKey).To(Equal(expectedKey))
			})
		})

		Context("when the tags are invalid", func() {
			Context("when the tags have no tags", func() {
				BeforeEach(func() {
					tags = nil
				})

				It("reports an error that the tags are missing", func() {
					Expect(keyConversionErr).To(MatchError(rep.ErrContainerMissingTags))
				})
			})

			Context("when the tags are missing the process guid tag ", func() {
				BeforeEach(func() {
					delete(tags, rep.ProcessGuidTag)
				})

				It("reports the process_guid is invalid", func() {
					Expect(keyConversionErr).To(HaveOccurred())
					Expect(keyConversionErr.Error()).To(ContainSubstring("process_guid"))
				})
			})

			Context("when the tags process index tag is not a number", func() {
				BeforeEach(func() {
					tags[rep.ProcessIndexTag] = "hi there"
				})

				It("reports the index is invalid when constructing ActualLRPKey", func() {
					Expect(keyConversionErr).To(MatchError(rep.ErrInvalidProcessIndex))
				})
			})

			Context("when the tag's process index overflows", func() {
				BeforeEach(func() {
					tags[rep.ProcessIndexTag] = fmt.Sprint(math.MaxInt32 + 1)
				})

				It("reports an integer overflow", func() {
					Expect(keyConversionErr).To(MatchError(rep.ErrIntergerOverflow))
				})
			})
		})
	})

	Describe("ActualLRPInstanceKeyFromContainer", func() {
		var (
			container                executor.Container
			lrpInstanceKey           *models.ActualLRPInstanceKey
			instanceKeyConversionErr error
			cellID                   string
		)

		BeforeEach(func() {
			container = executor.Container{
				Guid: "container-guid",
				Tags: executor.Tags{
					rep.LifecycleTag:    rep.LRPLifecycle,
					rep.DomainTag:       "my-domain",
					rep.ProcessGuidTag:  "process-guid",
					rep.ProcessIndexTag: "999",
					rep.InstanceGuidTag: "some-instance-guid",
				},
				RunInfo: executor.RunInfo{
					Ports: []executor.PortMapping{
						{
							ContainerPort: 1234,
							HostPort:      6789,
						},
					},
				},
			}
			cellID = "the-cell-id"
		})

		JustBeforeEach(func() {
			lrpInstanceKey, instanceKeyConversionErr = rep.ActualLRPInstanceKeyFromContainer(container, cellID)
		})

		Context("when the container and cell id are valid", func() {
			It("it does not return an error", func() {
				Expect(instanceKeyConversionErr).NotTo(HaveOccurred())
			})

			It("it creates the correct container key", func() {
				expectedInstanceKey := models.ActualLRPInstanceKey{
					InstanceGuid: "some-instance-guid",
					CellId:       cellID,
				}

				Expect(*lrpInstanceKey).To(Equal(expectedInstanceKey))
			})
		})

		Context("when the container is invalid", func() {
			Context("when the container has no tags", func() {
				BeforeEach(func() {
					container.Tags = nil
				})

				It("reports an error that the tags are missing", func() {
					Expect(instanceKeyConversionErr).To(MatchError(rep.ErrContainerMissingTags))
				})
			})

			Context("when the container is missing the instance guid tag ", func() {
				BeforeEach(func() {
					delete(container.Tags, rep.InstanceGuidTag)
				})

				It("returns an invalid instance-guid error", func() {
					Expect(instanceKeyConversionErr.Error()).To(ContainSubstring("instance_guid"))
				})
			})

			Context("when the cell id is invalid", func() {
				BeforeEach(func() {
					cellID = ""
				})

				It("returns an invalid cell id error", func() {
					Expect(instanceKeyConversionErr.Error()).To(ContainSubstring("cell_id"))
				})
			})
		})
	})

	Describe("ActualLRPNetInfoFromContainer", func() {
		var (
			container            executor.Container
			lrpNetInfo           *models.ActualLRPNetInfo
			netInfoConversionErr error
		)

		BeforeEach(func() {
			container = executor.Container{
				Guid:                                  "some-instance-guid",
				ExternalIP:                            "some-external-ip",
				InternalIP:                            "container-ip",
				AdvertisePreferenceForInstanceAddress: true,
				Tags: executor.Tags{
					rep.LifecycleTag:    rep.LRPLifecycle,
					rep.DomainTag:       "my-domain",
					rep.ProcessGuidTag:  "process-guid",
					rep.ProcessIndexTag: "999",
				},
				RunInfo: executor.RunInfo{
					Ports: []executor.PortMapping{
						{
							ContainerPort: 1234,
							HostPort:      6789,
						},
					},
				},
			}
		})

		JustBeforeEach(func() {
			lrpNetInfo, netInfoConversionErr = rep.ActualLRPNetInfoFromContainer(container)
		})

		Context("when container and executor host are valid", func() {
			It("does not return an error", func() {
				Expect(netInfoConversionErr).NotTo(HaveOccurred())
			})

			It("returns the correct net info", func() {
				expectedNetInfo := models.ActualLRPNetInfo{
					Ports: []*models.PortMapping{
						{
							ContainerPort: 1234,
							HostPort:      6789,
						},
					},
					Address:          "some-external-ip",
					InstanceAddress:  "container-ip",
					PreferredAddress: models.ActualLRPNetInfo_PreferredAddressInstance,
				}

				Expect(*lrpNetInfo).To(Equal(expectedNetInfo))
			})

			Context("when the container has an IPv6 address", func() {
				BeforeEach(func() {
					container.InternalIPv6 = "fd00::1"
				})

				It("includes the IPv6 address in the net info", func() {
					Expect(netInfoConversionErr).NotTo(HaveOccurred())
					Expect(lrpNetInfo.InstanceIpv6Address).To(Equal("fd00::1"))
				})
			})

			Context("when advertisePreferenceForInstanceAddress set to false", func() {
				BeforeEach(func() {
					container.AdvertisePreferenceForInstanceAddress = false
				})

				It("sets PreferredAddress as host", func() {
					Expect((*lrpNetInfo).PreferredAddress).To(Equal(models.ActualLRPNetInfo_PreferredAddressHost))
				})
			})
		})

		Context("when there are no exposed ports", func() {
			BeforeEach(func() {
				container.Ports = nil
			})

			It("does not return an error", func() {
				Expect(netInfoConversionErr).NotTo(HaveOccurred())
			})
		})

		Context("when the executor host is invalid", func() {
			BeforeEach(func() {
				container.ExternalIP = ""
			})

			It("returns an invalid host error", func() {
				Expect(netInfoConversionErr.Error()).To(ContainSubstring("address"))
			})
		})
	})

	Describe("RunRequestConversionHelper", func() {
		var (
			runRequestConversionHelper rep.RunRequestConversionHelper
			fakeECRHelper              *fakeecrhelper.FakeECRHelper
		)

		BeforeEach(func() {
			fakeECRHelper = &fakeecrhelper.FakeECRHelper{}
			runRequestConversionHelper = rep.RunRequestConversionHelper{
				ECRHelper: fakeECRHelper,
			}
		})

		Describe("NewRunRequestFromDesiredLRP", func() {
			var (
				containerGuid      string
				desiredLRP         *models.DesiredLRP
				desiredLRPNoVolume *models.DesiredLRP
				actualLRP          *models.ActualLRP
				stackPathMap       rep.StackPathMap
				runInfo            executor.RunInfo
			)

			BeforeEach(func() {
				containerGuid = "the-container-guid"
				desiredLRP = model_helpers.NewValidDesiredLRP("the-process-guid")
				desiredLRPNoVolume = model_helpers.NewValidDesiredLRPWithNoVolumeMountedFiles("the-process-guid")
				desiredLRP.Ports = []uint32{8080}
				// This is a lazy way to prevent old tests from failing.  The tests
				// happily ignored ImageLayer that used to be returned from
				// NewValidDesiredLRP, but now we are converting to V2 they are
				// failing because they are getting extra CachedDependencies.  We
				// test explicitly for V2 conversion in a context below
				desiredLRP.ImageLayers = nil
				actualLRP = model_helpers.NewValidActualLRP("the-process-guid", 9)
				desiredLRP.RootFs = "preloaded:cflinuxfs3"

				stackPathMap = rep.StackPathMap{
					"cflinuxfs3": "cflinuxfs3:/var/vcap/packages/cflinuxfs3/rootfs.tar",
				}

				desiredLRP.Sidecars = []*models.Sidecar{
					{
						Action:   models.WrapAction(&models.RunAction{Path: "sidecar-1"}),
						MemoryMb: 3,
						DiskMb:   4,
					},
					{
						Action:   models.WrapAction(&models.RunAction{Path: "sidecar-2"}),
						MemoryMb: 5,
						DiskMb:   6,
					},
				}

				desiredLRPNoVolume.Ports = desiredLRP.Ports
				desiredLRPNoVolume.ImageLayers = nil
				desiredLRPNoVolume.RootFs = desiredLRP.RootFs
				desiredLRPNoVolume.Sidecars = desiredLRP.Sidecars

				runInfo = executor.RunInfo{
					RootFSPath: stackPathMap["cflinuxfs3"],
					CPUWeight:  uint(desiredLRP.CpuWeight),
					Ports:      rep.ConvertPortMappings(desiredLRP.Ports),
					LogConfig: models.LogConfig{
						Guid:       desiredLRP.LogGuid,
						Index:      int(actualLRP.Index),
						SourceName: desiredLRP.LogSource,
						Tags: map[string]string{
							"source_id": "some-metrics-guid",
						},
					},
					MetricsConfig: executor.MetricsConfig{
						Guid:  desiredLRP.MetricsGuid,
						Index: int(actualLRP.Index),
						Tags: map[string]string{
							"source_id": "some-metrics-guid",
						},
					},
					StartTimeoutMs: uint(desiredLRP.StartTimeoutMs),
					Privileged:     desiredLRP.Privileged,
					CachedDependencies: []executor.CachedDependency{
						{Name: "app bits", From: "blobstore.com/bits/app-bits", To: "/usr/local/app", CacheKey: "cache-key", LogSource: "log-source"},
						{Name: "app bits with checksum", From: "blobstore.com/bits/app-bits-checksum", To: "/usr/local/app-checksum", CacheKey: "cache-key", LogSource: "log-source", ChecksumAlgorithm: "md5", ChecksumValue: "checksum-value"},
					},
					Setup:           desiredLRP.Setup,
					Action:          desiredLRP.Action,
					Monitor:         desiredLRP.Monitor,
					CheckDefinition: desiredLRP.CheckDefinition,
					EgressRules:     desiredLRP.EgressRules,
					Env: append([]executor.EnvironmentVariable{
						{Name: "INSTANCE_GUID", Value: actualLRP.InstanceGuid},
						{Name: "INSTANCE_INDEX", Value: strconv.Itoa(int(actualLRP.Index))},
						{Name: "CF_INSTANCE_GUID", Value: actualLRP.InstanceGuid},
						{Name: "CF_INSTANCE_INDEX", Value: strconv.Itoa(int(actualLRP.Index))},
					}, executor.EnvironmentVariablesFromModel(desiredLRP.EnvironmentVariables)...),
					TrustedSystemCertificatesPath: "/etc/somepath",
					VolumeMounts: []executor.VolumeMount{
						{
							Driver:        "my-driver",
							VolumeId:      "my-volume",
							ContainerPath: "/mnt/mypath",
							Config:        map[string]interface{}{"foo": "bar"},
							Mode:          executor.BindMountModeRO,
						},
					},
					Network: &executor.Network{
						Properties: map[string]string{
							"some-key":       "some-value",
							"some-other-key": "some-other-value",
						},
					},
					CertificateProperties: executor.CertificateProperties{
						OrganizationalUnit: []string{"iamthelizardking", "iamthelizardqueen"},
					},
					ImageUsername:        "image-username",
					ImagePassword:        "image-password",
					EnableContainerProxy: true,
					Sidecars: []executor.Sidecar{
						{
							Action:   desiredLRP.Sidecars[0].Action,
							MemoryMB: 3,
							DiskMB:   4,
						},
						{
							Action:   desiredLRP.Sidecars[1].Action,
							MemoryMB: 5,
							DiskMB:   6,
						},
					},
					LogRateLimitBytesPerSecond: -1,
					VolumeMountedFiles:         []executor.VolumeMountedFiles{{Path: "/redis/username", Content: "redis_user"}},
				}
			})

			It("returns a valid run request", func() {
				runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
				Expect(err).NotTo(HaveOccurred())
				Expect(runReq.Tags).To(Equal(executor.Tags{}))

				Expect(runReq.RunInfo).To(test_helpers.DeepEqual(runInfo))

			})

			It("return a valid run request with no volume mounted files", func() {
				runReqNoVolume, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRPNoVolume, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
				Expect(err).NotTo(HaveOccurred())

				Expect(runReqNoVolume.Tags).To(Equal(executor.Tags{}))

				runInfo.VolumeMountedFiles = []executor.VolumeMountedFiles{}
				Expect(runReqNoVolume.RunInfo).To(test_helpers.DeepEqual(runInfo))

			})

			Context("when the network is nil", func() {
				BeforeEach(func() {
					desiredLRP.Network = nil
				})

				It("sets a nil network on the result", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.Network).To(BeNil())
				})
			})

			Context("when the certificate properties are nil", func() {
				BeforeEach(func() {
					desiredLRP.CertificateProperties = nil
				})

				It("it sets an empty certificate properties on the result", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.CertificateProperties).To(Equal(executor.CertificateProperties{}))
				})
			})

			Context("when a volumeMount config is invalid", func() {
				BeforeEach(func() {
					desiredLRP.VolumeMounts[0].Shared.MountConfig = "{{"
				})

				It("returns an error", func() {
					_, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).To(HaveOccurred())
				})
			})

			Context("when a volumeMount is dedicated", func() {
				BeforeEach(func() {
					desiredLRP.VolumeMounts[0] = &models.VolumeMount{
						Dedicated: &models.DedicatedDevice{
							MounterId:    "my-mounter-id",
							MountConfig:  `{"foo":"bar"}`,
							DeviceConfig: `{"baz":"qux"}`,
						},
						Driver:       "my-driver",
						ContainerDir: "/mnt/mypath",
						Mode:         "r",
					}
				})

				It("returns a dedicated volume mount", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.VolumeMounts).To(ConsistOf(executor.VolumeMount{
						Driver:        "my-driver",
						VolumeId:      "my-mounter-id-9",
						ContainerPath: "/mnt/mypath",
						Config: map[string]interface{}{
							"mount_config":  map[string]interface{}{"foo": "bar"},
							"device_config": map[string]interface{}{"baz": "qux"},
							"source":        "my-mounter-id-9",
						},
						Mode: executor.BindMountModeRO,
					}))
				})

				Context("when MountConfig is invalid JSON", func() {
					BeforeEach(func() {
						desiredLRP.VolumeMounts[0].Dedicated.MountConfig = "{{"
					})

					It("returns an error", func() {
						_, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
						Expect(err).To(HaveOccurred())
					})
				})

				Context("when DeviceConfig is invalid JSON", func() {
					BeforeEach(func() {
						desiredLRP.VolumeMounts[0].Dedicated.DeviceConfig = "{{"
					})

					It("returns an error", func() {
						_, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
						Expect(err).To(HaveOccurred())
					})
				})
			})

			It("enables the envoy proxy", func() {
				runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
				Expect(err).NotTo(HaveOccurred())
				Expect(runReq.EnableContainerProxy).To(BeTrue())
			})

			Context("when the LRP doesn't have any exposed ports", func() {
				BeforeEach(func() {
					desiredLRP.Ports = nil
				})

				It("disables the envoy proxy", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.EnableContainerProxy).To(BeFalse())
				})
			})

			Context("when the rootfs is preloaded+layer", func() {
				BeforeEach(func() {
					desiredLRP.RootFs = "preloaded+layer:cflinuxfs3?layer=http://file-server/layer.tgz&layer_digest=some-digest&layer_path=/path/in/container"
				})

				It("enables the envoy proxy", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.EnableContainerProxy).To(BeTrue())
				})

				It("uses TotalDiskLimit as the disk scope", func() {
					_, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			Context("when the rootfs is not preloaded", func() {
				BeforeEach(func() {
					desiredLRP.RootFs = "docker://cloudfoundry/test"
				})

				It("enables the envoy proxy", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.EnableContainerProxy).To(BeTrue())
				})

				It("uses TotalDiskLimit as the disk scope", func() {
					_, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			Context("when the rootfs is ECR repo URL", func() {
				BeforeEach(func() {
					fakeECRHelper.IsECRRepoReturns(true, nil)
					fakeECRHelper.GetECRCredentialsReturns("some-docker-username", "some-docker-password", nil)
				})

				It("sets username and password to ECR provided username and password", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.ImageUsername).To(Equal("some-docker-username"))
					Expect(runReq.ImagePassword).To(Equal("some-docker-password"))
				})

				Context("when checking for ECR repo fails", func() {
					BeforeEach(func() {
						fakeECRHelper.IsECRRepoReturns(false, errors.New("disaster"))
					})

					It("returns an error", func() {
						_, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
						Expect(err).To(HaveOccurred())
					})
				})

				Context("when getting ECR credentials fails", func() {
					BeforeEach(func() {
						fakeECRHelper.IsECRRepoReturns(true, nil)
						fakeECRHelper.GetECRCredentialsReturns("", "", errors.New("disaster"))
					})

					It("returns an error", func() {
						_, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
						Expect(err).To(HaveOccurred())
					})
				})
			})

			Context("when the LRP's CpuWeight is 0", func() {
				BeforeEach(func() {
					desiredLRP.CpuWeight = 0
				})

				It("sets the runInfo's CPUWeight to 1, because 0 is invalid per the spec", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())

					Expect(runReq.RunInfo.CPUWeight).To(Equal(uint(1)))
				})
			})

			Context("when the lrp has V3 declarative Resources", func() {
				var origSetup *models.Action

				BeforeEach(func() {
					desiredLRP.CachedDependencies = nil
					origSetup = desiredLRP.Setup
					desiredLRP.ImageLayers = []*models.ImageLayer{
						{
							Name:            "app bits",
							Url:             "blobstore.com/bits/app-bits",
							DestinationPath: "/usr/local/app",
							LayerType:       models.LayerTypeShared,
							MediaType:       models.MediaTypeTgz,
							DigestAlgorithm: models.DigestAlgorithmSha256,
							DigestValue:     "some-sha256",
						},
						{
							Name:            "other bits with checksum",
							Url:             "blobstore.com/bits/other-bits-checksum",
							DestinationPath: "/usr/local/other",
							LayerType:       models.LayerTypeExclusive,
							MediaType:       models.MediaTypeTgz,
							DigestAlgorithm: models.DigestAlgorithmSha256,
							DigestValue:     "some-other-sha256",
						},
					}
				})

				It("converts exclusive resources into download steps", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.Setup).To(Equal(models.WrapAction(models.Serial(
						models.Parallel(
							&models.DownloadAction{
								Artifact:          "other bits with checksum",
								From:              "blobstore.com/bits/other-bits-checksum",
								To:                "/usr/local/other",
								CacheKey:          "sha256:some-other-sha256",
								LogSource:         "",
								User:              "legacy-dan",
								ChecksumAlgorithm: "sha256",
								ChecksumValue:     "some-other-sha256",
							},
						),
						models.UnwrapAction(origSetup),
					))))
				})

				It("converts shared resources into V2 cached dependencies", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.CachedDependencies).To(ConsistOf(executor.CachedDependency{
						Name:              "app bits",
						From:              "blobstore.com/bits/app-bits",
						To:                "/usr/local/app",
						CacheKey:          "sha256:some-sha256",
						LogSource:         "",
						ChecksumAlgorithm: "sha256",
						ChecksumValue:     "some-sha256",
					}))
				})

				Context("when the layering mode is 'two-layer'", func() {
					It("converts the rootfs to a preloaded+layer scheme, and first exclusive resource into the extra layer in the rootfs", func() {
						runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeTwoLayer)
						Expect(err).NotTo(HaveOccurred())

						Expect(runReq.RootFSPath).To(Equal(fmt.Sprintf(
							"preloaded+layer:%s?layer=%s&layer_path=%s&layer_digest=%s",
							stackPathMap["cflinuxfs3"],
							url.QueryEscape(desiredLRP.ImageLayers[1].Url),
							url.QueryEscape(desiredLRP.ImageLayers[1].DestinationPath),
							url.QueryEscape(desiredLRP.ImageLayers[1].DigestValue),
						)))
					})

					It("excludes the converted exclusive resource from the download steps", func() {
						runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeTwoLayer)
						Expect(err).NotTo(HaveOccurred())
						Expect(runReq.Setup).To(Equal(origSetup))
					})

					It("converts shared resources into V2 cached dependencies", func() {
						runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeTwoLayer)
						Expect(err).NotTo(HaveOccurred())
						Expect(runReq.CachedDependencies).To(ConsistOf(executor.CachedDependency{
							Name:              "app bits",
							From:              "blobstore.com/bits/app-bits",
							To:                "/usr/local/app",
							CacheKey:          "sha256:some-sha256",
							LogSource:         "",
							ChecksumAlgorithm: "sha256",
							ChecksumValue:     "some-sha256",
						}))
					})
				})
			})

			Context("when metric tags are specified", func() {
				BeforeEach(func() {
					desiredLRP.MetricTags = map[string]*models.MetricTagValue{
						"tag1":         {Static: "foo"},
						"index":        {Dynamic: models.MetricTagDynamicValueIndex},
						"instanceGuid": {Dynamic: models.MetricTagDynamicValueInstanceGuid},
					}
				})

				It("converts static tags directly to the provided string", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.RunInfo.MetricsConfig.Tags).To(HaveKeyWithValue("tag1", "foo"))
					Expect(runReq.RunInfo.LogConfig.Tags).To(HaveKeyWithValue("tag1", "foo"))
				})

				It("populates tags with dynamic values according to its enum value", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.RunInfo.MetricsConfig.Tags).To(HaveKeyWithValue("index", "9"))
					Expect(runReq.RunInfo.MetricsConfig.Tags).To(HaveKeyWithValue("instanceGuid", "some-guid"))
					Expect(runReq.RunInfo.LogConfig.Tags).To(HaveKeyWithValue("index", "9"))
					Expect(runReq.RunInfo.LogConfig.Tags).To(HaveKeyWithValue("instanceGuid", "some-guid"))
				})
			})

			Context("when desired LRP has internal routes", func() {
				BeforeEach(func() {
					myRouterJSON := json.RawMessage(`[{"hostname":"foo.apps.internal"},{"hostname":"bar.apps.internal"}]`)
					desiredLRP.Routes = &models.Routes{"internal-router": &myRouterJSON}
				})

				It("adds internal routes to run info", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.RunInfo.InternalRoutes).To(Equal(models.InternalRoutes{
						{Hostname: "foo.apps.internal"},
						{Hostname: "bar.apps.internal"},
					}))
				})
			})

			Describe("the execution backend", func() {
				BeforeEach(func() {
					// The default sidecars do not run through the launcher, which
					// a VM backend refuses.
					desiredLRP.Sidecars = nil
				})

				runRequest := func() executor.RunRequest {
					runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					return runReq
				}

				Context("when the CF_EXECUTION_BACKEND environment variable is set", func() {
					BeforeEach(func() {
						desiredLRP.EnvironmentVariables = append(desiredLRP.EnvironmentVariables,
							&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "vm-qemu"})
					})

					It("sets the execution backend on the run info", func() {
						Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("vm-qemu"))
					})
				})

				Context("when the CF_EXECUTION_BACKEND environment variable is set more than once", func() {
					BeforeEach(func() {
						desiredLRP.EnvironmentVariables = append(desiredLRP.EnvironmentVariables,
							&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "gvisor"},
							&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "vm-qemu"})
					})

					It("uses the last value", func() {
						Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("vm-qemu"))
					})
				})

				Context("when the CF_EXECUTION_BACKEND environment variable is absent", func() {
					It("leaves the execution backend empty", func() {
						Expect(runRequest().RunInfo.ExecutionBackend).To(BeEmpty())
					})
				})

				Describe("as Cloud Controller builds the LRP", func() {
					backendEnv := func(value string) []*models.EnvironmentVariable {
						return []*models.EnvironmentVariable{
							{Name: "VCAP_APPLICATION", Value: "{}"},
							{Name: "CF_EXECUTION_BACKEND", Value: value},
						}
					}
					launcher := func(env ...*models.EnvironmentVariable) *models.RunAction {
						return &models.RunAction{Path: "/tmp/lifecycle/launcher", Args: []string{"app", "", ""}, Env: env}
					}
					wrap := func(action models.ActionInterface) *models.Action { return models.WrapAction(action) }

					BeforeEach(func() {
						// Cloud Controller puts only LANG in the LRP-level
						// environment. The app environment lives in the env of
						// the RunAction that starts the app.
						desiredLRP.EnvironmentVariables = []*models.EnvironmentVariable{{Name: "LANG", Value: "en_US.UTF-8"}}
					})

					Context("when the variable is only in the env of the launcher RunAction", func() {
						BeforeEach(func() {
							desiredLRP.Action = wrap(launcher(backendEnv("vm-qemu")...))
						})

						It("selects that backend", func() {
							Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("vm-qemu"))
						})
					})

					Context("when the launcher RunAction is nested in a codependent action", func() {
						BeforeEach(func() {
							desiredLRP.Action = wrap(models.Codependent(
								&models.RunAction{Path: "/tmp/other"},
								launcher(backendEnv("vm-firecracker")...),
							))
						})

						It("selects that backend", func() {
							Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("vm-firecracker"))
						})
					})

					DescribeTable("when the launcher RunAction is nested in a wrapper action",
						func(build func(inner *models.Action) models.ActionInterface) {
							desiredLRP.Action = wrap(build(wrap(launcher(backendEnv("gvisor")...))))
							Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("gvisor"))
						},
						Entry("serial", func(inner *models.Action) models.ActionInterface {
							return &models.SerialAction{Actions: []*models.Action{wrap(&models.RunAction{Path: "/tmp/other"}), inner}}
						}),
						Entry("parallel", func(inner *models.Action) models.ActionInterface {
							return &models.ParallelAction{Actions: []*models.Action{inner}}
						}),
						Entry("codependent", func(inner *models.Action) models.ActionInterface {
							return &models.CodependentAction{Actions: []*models.Action{inner}}
						}),
						Entry("timeout", func(inner *models.Action) models.ActionInterface {
							return models.Timeout(models.UnwrapAction(inner), time.Minute)
						}),
						Entry("try", func(inner *models.Action) models.ActionInterface {
							return &models.TryAction{Action: inner}
						}),
						Entry("emit progress", func(inner *models.Action) models.ActionInterface {
							return &models.EmitProgressAction{Action: inner}
						}),
						Entry("every wrapper at once", func(inner *models.Action) models.ActionInterface {
							return models.Codependent(models.Serial(models.EmitProgressFor(models.Timeout(models.Try(models.Parallel(models.UnwrapAction(inner))), time.Minute), "a", "b", "c")))
						}),
					)

					Context("when a RunAction that is not the launcher carries the variable", func() {
						BeforeEach(func() {
							desiredLRP.Action = wrap(models.Codependent(
								&models.RunAction{Path: "/tmp/other", Env: backendEnv("vm-qemu")},
								launcher(backendEnv("gvisor")...),
							))
						})

						It("prefers the launcher RunAction", func() {
							Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("gvisor"))
						})
					})

					Context("when only a RunAction that is not the launcher carries the variable", func() {
						BeforeEach(func() {
							desiredLRP.Action = wrap(&models.RunAction{Path: "/tmp/other", Env: backendEnv("vm-qemu")})
						})

						It("leaves the execution backend empty", func() {
							Expect(runRequest().RunInfo.ExecutionBackend).To(BeEmpty())
						})
					})

					Context("when the launcher RunAction has no variable but the LRP-level environment does", func() {
						BeforeEach(func() {
							desiredLRP.Action = wrap(launcher())
							desiredLRP.EnvironmentVariables = backendEnv("vm-qemu")
						})

						It("falls back to the LRP-level environment", func() {
							Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("vm-qemu"))
						})
					})

					Context("when the launcher RunAction repeats the variable", func() {
						BeforeEach(func() {
							desiredLRP.Action = wrap(launcher(append(backendEnv("gvisor"), backendEnv("vm-qemu")...)...))
						})

						It("uses the last value", func() {
							Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("vm-qemu"))
						})
					})

					Context("when the action tree is empty or has nil entries", func() {
						BeforeEach(func() {
							desiredLRP.Action = wrap(&models.SerialAction{Actions: []*models.Action{nil, {}}})
						})

						It("leaves the execution backend empty", func() {
							Expect(runRequest().RunInfo.ExecutionBackend).To(BeEmpty())
						})
					})

					Describe("sidecars", func() {
						sidecarRequest := func() (executor.RunRequest, error) {
							return runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
						}

						// These names are the kinds guardian's backend package
						// treats as VMs. A rename or a new kind there has to
						// change this list and isVMBackend together.
						DescribeTable("which backend names count as a VM",
							func(name string, folded bool) {
								var env []*models.EnvironmentVariable
								if name != "" {
									env = backendEnv(name)
								}
								desiredLRP.Action = wrap(launcher(env...))
								desiredLRP.Sidecars = []*models.Sidecar{{Action: wrap(launcher(env...))}}

								runReq, err := sidecarRequest()
								Expect(err).NotTo(HaveOccurred())
								Expect(runReq.RunInfo.Sidecars).To(HaveLen(1))
								relay, isRun := models.UnwrapAction(runReq.RunInfo.Sidecars[0].Action).(*models.RunAction)
								Expect(isRun).To(BeTrue())
								if folded {
									Expect(relay.Env).To(HaveLen(1))
									Expect(relay.Env[0].Name).To(Equal("CF_VM_SIDECAR_RELAY"))
								} else {
									for _, v := range relay.Env {
										Expect(v.Name).NotTo(Equal("CF_VM_SIDECAR_RELAY"))
									}
								}
							},
							Entry("vm-qemu", "vm-qemu", true),
							Entry("vm-firecracker", "vm-firecracker", true),
							Entry("oci-gvisor", "oci-gvisor", false),
							Entry("no backend", "", false),
						)

						Context("when a launcher sidecar is added to a VM backend LRP", func() {
							BeforeEach(func() {
								desiredLRP.Action = wrap(launcher(backendEnv("vm-qemu")...))
								desiredLRP.Sidecars = []*models.Sidecar{{Action: wrap(launcher(backendEnv("vm-qemu")...))}}
							})

							It("folds the sidecar into the app's launcher action", func() {
								runReq, err := sidecarRequest()
								Expect(err).NotTo(HaveOccurred())
								Expect(runReq.RunInfo.Sidecars).To(HaveLen(1))
								run := models.UnwrapAction(runReq.RunInfo.Action).(*models.RunAction)
								Expect(run.Env[len(run.Env)-2].Name).To(Equal("CF_VM_SIDECARS"))
							})
						})

						Context("when the VM backend LRP runs two launcher processes in its action", func() {
							BeforeEach(func() {
								desiredLRP.Action = wrap(models.Codependent(
									launcher(backendEnv("vm-firecracker")...),
									launcher(backendEnv("vm-firecracker")...),
								))
							})

							It("folds the second one as a sidecar", func() {
								runReq, err := sidecarRequest()
								Expect(err).NotTo(HaveOccurred())
								codependent := models.UnwrapAction(runReq.RunInfo.Action).(*models.CodependentAction)
								Expect(codependent.Actions).To(HaveLen(2))
								relay := models.UnwrapAction(codependent.Actions[1]).(*models.RunAction)
								Expect(relay.Env[0].Name).To(Equal("CF_VM_SIDECAR_RELAY"))
							})
						})

						Context("when a launcher process is nested in a wrapper of the action", func() {
							BeforeEach(func() {
								desiredLRP.Action = wrap(models.Codependent(
									launcher(backendEnv("vm-firecracker")...),
									models.Serial(launcher(backendEnv("vm-firecracker")...)),
								))
							})

							It("fails the conversion", func() {
								_, err := sidecarRequest()
								Expect(err).To(MatchError(rep.ErrVMBackendSidecars))
							})
						})

						Context("when the backend comes from the LRP-level environment and a sidecar exists", func() {
							BeforeEach(func() {
								desiredLRP.Action = wrap(launcher())
								desiredLRP.EnvironmentVariables = backendEnv("vm-qemu")
								desiredLRP.Sidecars = []*models.Sidecar{{Action: wrap(launcher())}}
							})

							It("folds the sidecar", func() {
								runReq, err := sidecarRequest()
								Expect(err).NotTo(HaveOccurred())
								Expect(runReq.RunInfo.ExecutionBackend).To(Equal("vm-qemu"))
								Expect(runReq.RunInfo.Sidecars).To(HaveLen(1))
							})
						})

						Context("when a sidecar is not run through the launcher", func() {
							BeforeEach(func() {
								desiredLRP.Action = wrap(launcher(backendEnv("vm-qemu")...))
								desiredLRP.Sidecars = []*models.Sidecar{{Action: wrap(&models.RunAction{Path: "/tmp/other"})}}
							})

							It("fails the conversion", func() {
								_, err := sidecarRequest()
								Expect(err).To(MatchError(rep.ErrVMBackendSidecars))
							})
						})

						Context("when a gvisor LRP has a launcher sidecar", func() {
							BeforeEach(func() {
								desiredLRP.Action = wrap(launcher(backendEnv("gvisor")...))
								desiredLRP.Sidecars = []*models.Sidecar{{Action: wrap(launcher(backendEnv("gvisor")...))}}
							})

							It("selects the backend", func() {
								runReq, err := sidecarRequest()
								Expect(err).NotTo(HaveOccurred())
								Expect(runReq.RunInfo.ExecutionBackend).To(Equal("gvisor"))
							})
						})

						Context("when a default-runtime LRP has a launcher sidecar", func() {
							BeforeEach(func() {
								desiredLRP.Action = wrap(launcher())
								desiredLRP.Sidecars = []*models.Sidecar{{Action: wrap(launcher())}}
							})

							It("leaves the execution backend empty", func() {
								runReq, err := sidecarRequest()
								Expect(err).NotTo(HaveOccurred())
								Expect(runReq.RunInfo.ExecutionBackend).To(BeEmpty())
							})
						})
					})

					Describe("the rootfs of a VM backend", func() {
						rootFSRequest := func() (executor.RunRequest, error) {
							return runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeSingleLayer)
						}

						BeforeEach(func() {
							desiredLRP.Sidecars = nil
						})

						DescribeTable("which rootfs a backend accepts",
							func(backendName, rootFS string, rejected bool) {
								desiredLRP.RootFs = rootFS
								desiredLRP.Action = wrap(launcher(backendEnv(backendName)...))

								runReq, err := rootFSRequest()
								if rejected {
									Expect(err).To(MatchError(rep.ErrVMBackendRootFS))
									Expect(err).To(MatchError(ContainSubstring(fmt.Sprintf("backend %q", backendName))))
								} else {
									Expect(err).NotTo(HaveOccurred())
									Expect(runReq.RunInfo.ExecutionBackend).To(Equal(backendName))
								}
							},
							Entry("vm-qemu with a preloaded stack", "vm-qemu", "preloaded:cflinuxfs3", false),
							Entry("vm-qemu with a preloaded stack and a droplet layer", "vm-qemu", "preloaded+layer:cflinuxfs3?layer=http://file-server/layer.tgz&layer_digest=some-digest&layer_path=/home/vcap", false),
							Entry("vm-qemu with a droplet layer below /home/vcap", "vm-qemu", "preloaded+layer:cflinuxfs3?layer=http://file-server/layer.tgz&layer_digest=some-digest&layer_path=/home/vcap/app", false),
							Entry("vm-qemu with a droplet layer at /etc", "vm-qemu", "preloaded+layer:cflinuxfs3?layer=http://file-server/layer.tgz&layer_digest=some-digest&layer_path=/etc", true),
							Entry("vm-qemu with a droplet layer escaping /home/vcap", "vm-qemu", "preloaded+layer:cflinuxfs3?layer=http://file-server/layer.tgz&layer_digest=some-digest&layer_path=/home/vcap/../../etc", true),
							Entry("vm-qemu with a droplet layer and no layer_path", "vm-qemu", "preloaded+layer:cflinuxfs3?layer=http://file-server/layer.tgz&layer_digest=some-digest", true),
							Entry("vm-firecracker with a preloaded stack", "vm-firecracker", "preloaded:cflinuxfs3", false),
							Entry("vm-qemu with a docker image", "vm-qemu", "docker:///cloudfoundry/test", false),
							Entry("vm-firecracker with a docker image", "vm-firecracker", "docker://registry.example.com/cloudfoundry/test", false),
							Entry("vm-qemu with an unknown scheme", "vm-qemu", "oci:///some/image", true),
							Entry("vm-qemu with no rootfs", "vm-qemu", "", true),
							Entry("oci-gvisor with a docker image", "oci-gvisor", "docker:///cloudfoundry/test", false),
							Entry("no backend with a docker image", "", "docker:///cloudfoundry/test", false),
						)

						It("names the scheme and never the rootfs URL", func() {
							desiredLRP.RootFs = "oci://some-user:some-secret@registry.example.com/cloudfoundry/test"
							desiredLRP.Action = wrap(launcher(backendEnv("vm-qemu")...))

							_, err := rootFSRequest()
							Expect(err).To(MatchError(ContainSubstring(`rootfs scheme "oci"`)))
							Expect(err.Error()).NotTo(ContainSubstring("some-secret"))
							Expect(err.Error()).NotTo(ContainSubstring("registry.example.com"))
						})

						It("converts a preloaded stack into a preloaded layer in two-layer mode and accepts it", func() {
							desiredLRP.RootFs = "preloaded:cflinuxfs3"
							desiredLRP.ImageLayers = []*models.ImageLayer{{
								Name:            "droplet",
								Url:             "http://file-server/droplet.tgz",
								DestinationPath: "/home/vcap",
								LayerType:       models.LayerTypeExclusive,
								MediaType:       models.MediaTypeTgz,
								DigestAlgorithm: models.DigestAlgorithmSha256,
								DigestValue:     "some-digest",
							}}
							desiredLRP.Action = wrap(launcher(backendEnv("vm-qemu")...))

							runReq, err := runRequestConversionHelper.NewRunRequestFromDesiredLRP(containerGuid, desiredLRP, &actualLRP.ActualLRPKey, &actualLRP.ActualLRPInstanceKey, stackPathMap, rep.LayeringModeTwoLayer)
							Expect(err).NotTo(HaveOccurred())
							Expect(runReq.RunInfo.RootFSPath).To(HavePrefix("preloaded+layer:"))
						})

						It("reports the sidecar limitation first when both apply", func() {
							desiredLRP.RootFs = "oci:///some/image"
							desiredLRP.Action = wrap(launcher(backendEnv("vm-qemu")...))
							desiredLRP.Sidecars = []*models.Sidecar{{Action: wrap(&models.RunAction{Path: "/tmp/other"})}}

							_, err := rootFSRequest()
							Expect(err).To(MatchError(rep.ErrVMBackendSidecars))
							Expect(err).NotTo(MatchError(rep.ErrVMBackendRootFS))
						})
					})
				})
			})
		})

		Describe("NewRunRequestFromTask", func() {
			var (
				task         *models.Task
				stackPathMap rep.StackPathMap
				runInfo      executor.RunInfo
			)

			BeforeEach(func() {
				task = model_helpers.NewValidTask("task-guid")
				// This is a lazy way to prevent old tests from failing.  The tests
				// happily ignored ImageLayer that used to be returned from
				// NewValidTask, but now we are converting to V2 they are failing
				// because they are getting extra CachedDependencies.  We test
				// explicitly for V2 conversion in a context below
				task.ImageLayers = nil
				task.RootFs = "preloaded:cflinuxfs3"
				task.VolumeMountedFiles = []*models.File{
					{Path: "/redis/username", Content: "username"},
				}

				stackPathMap = rep.StackPathMap{
					"cflinuxfs3": "cflinuxfs3:/var/vcap/packages/cflinuxfs3/rootfs.tar",
				}

				task.LogRateLimit = &models.LogRateLimit{BytesPerSecond: -1}

				runInfo = executor.RunInfo{
					RootFSPath: stackPathMap["cflinuxfs3"],
					CPUWeight:  uint(task.CpuWeight),
					Privileged: task.Privileged,
					CachedDependencies: []executor.CachedDependency{
						{Name: "app bits", From: "blobstore.com/bits/app-bits", To: "/usr/local/app", CacheKey: "cache-key", LogSource: "log-source"},
						{Name: "app bits with checksum", From: "blobstore.com/bits/app-bits-checksum", To: "/usr/local/app-checksum", CacheKey: "cache-key", LogSource: "log-source", ChecksumAlgorithm: "md5", ChecksumValue: "checksum-value"},
					},
					LogConfig: models.LogConfig{
						Guid:       task.LogGuid,
						SourceName: task.LogSource,
						Tags: map[string]string{
							"source_id": "some-metrics-guid",
						},
					},
					MetricsConfig: executor.MetricsConfig{
						Guid: task.MetricsGuid,
						Tags: map[string]string{
							"source_id": "some-metrics-guid",
						},
					},
					Action:                        task.Action,
					Env:                           executor.EnvironmentVariablesFromModel(task.EnvironmentVariables),
					EgressRules:                   task.EgressRules,
					TrustedSystemCertificatesPath: "/etc/somepath",
					VolumeMounts: []executor.VolumeMount{{
						Driver:        "my-driver",
						VolumeId:      "my-volume",
						ContainerPath: "/mnt/mypath",
						Config:        map[string]interface{}{"foo": "bar"},
						Mode:          executor.BindMountModeRO,
					}},
					Network: &executor.Network{
						Properties: map[string]string{
							"some-key":       "some-value",
							"some-other-key": "some-other-value",
						},
					},
					CertificateProperties: executor.CertificateProperties{
						OrganizationalUnit: []string{"iamthelizardking", "iamthelizardqueen"},
					},
					ImageUsername:              "image-username",
					ImagePassword:              "image-password",
					EnableContainerProxy:       false,
					LogRateLimitBytesPerSecond: -1,
					VolumeMountedFiles:         []executor.VolumeMountedFiles{{Path: "/redis/username", Content: "username"}},
				}
			})

			It("returns a valid run request", func() {
				runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
				Expect(err).NotTo(HaveOccurred())
				Expect(runReq.Tags).To(Equal(executor.Tags{
					rep.ResultFileTag: task.ResultFile,
				}))

				Expect(runReq.RunInfo).To(Equal(runInfo))
			})

			It("returns a valid run request with no volume mounted files", func() {
				task.VolumeMountedFiles = []*models.File{}
				runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)

				Expect(err).NotTo(HaveOccurred())
				Expect(runReq.Tags).To(Equal(executor.Tags{
					rep.ResultFileTag: task.ResultFile,
				}))

				runInfo.VolumeMountedFiles = []executor.VolumeMountedFiles{}

				Expect(runReq.RunInfo).To(Equal(runInfo))
			})

			Context("when the task tags contain dynamic fields not applicable to tasks", func() {
				BeforeEach(func() {
					task.MetricTags["index"] = &models.MetricTagValue{Dynamic: models.MetricTagDynamicValueIndex}
				})
				It("errors", func() {
					_, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).To(HaveOccurred())
				})
			})

			Context("when the network is nil", func() {
				BeforeEach(func() {
					task.Network = nil
				})

				It("sets a nil network on the result", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.Network).To(BeNil())
				})
			})

			Context("when the certificate properties are nil", func() {
				BeforeEach(func() {
					task.CertificateProperties = nil
				})

				It("it sets an empty certificate properties on the result", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.CertificateProperties).To(Equal(executor.CertificateProperties{}))
				})
			})

			It("disables the envoy proxy", func() {
				runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
				Expect(err).NotTo(HaveOccurred())
				Expect(runReq.EnableContainerProxy).To(BeFalse())
			})

			Context("when the rootfs is not preloaded", func() {
				BeforeEach(func() {
					task.RootFs = "docker://cloudfoundry/test"
				})

				It("disables the envoy proxy", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.EnableContainerProxy).To(BeFalse())
				})

				It("uses TotalDiskLimit as the disk scope", func() {
					_, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
				})
			})

			Context("when the rootfs is ECR repo URL", func() {
				BeforeEach(func() {
					fakeECRHelper.IsECRRepoReturns(true, nil)
					fakeECRHelper.GetECRCredentialsReturns("some-docker-username", "some-docker-password", nil)
				})

				It("sets username and password to ECR provided username and password", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.ImageUsername).To(Equal("some-docker-username"))
					Expect(runReq.ImagePassword).To(Equal("some-docker-password"))
				})

				Context("when checking for ECR repo fails", func() {
					BeforeEach(func() {
						fakeECRHelper.IsECRRepoReturns(false, errors.New("disaster"))
					})

					It("returns an error", func() {
						_, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
						Expect(err).To(HaveOccurred())
					})
				})

				Context("when getting ECR credentials fails", func() {
					BeforeEach(func() {
						fakeECRHelper.IsECRRepoReturns(true, nil)
						fakeECRHelper.GetECRCredentialsReturns("", "", errors.New("disaster"))
					})

					It("returns an error", func() {
						_, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
						Expect(err).To(HaveOccurred())
					})
				})
			})

			Context("when a volumeMount config is invalid", func() {
				BeforeEach(func() {
					task.VolumeMounts[0].Shared.MountConfig = "{{"
				})

				It("returns an error", func() {
					_, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).To(MatchError("invalid character '{' looking for beginning of object key string"))
				})
			})

			Context("when the task has V3 declarative Resources", func() {
				BeforeEach(func() {
					task.CachedDependencies = nil
					task.ImageLayers = []*models.ImageLayer{
						{
							Name:            "app bits",
							Url:             "blobstore.com/bits/app-bits",
							DestinationPath: "/usr/local/app",
							LayerType:       models.LayerTypeShared,
							MediaType:       models.MediaTypeTgz,
							DigestAlgorithm: models.DigestAlgorithmSha256,
							DigestValue:     "some-sha256",
						},
						{
							Name:            "other bits with checksum",
							Url:             "blobstore.com/bits/other-bits-checksum",
							DestinationPath: "/usr/local/other",
							LayerType:       models.LayerTypeExclusive,
							MediaType:       models.MediaTypeTgz,
							DigestAlgorithm: models.DigestAlgorithmSha256,
							DigestValue:     "some-other-sha256",
						},
					}
				})

				It("converts exclusive resources into run requests setup action", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.Setup).To(Equal(models.WrapAction(
						models.Parallel(
							&models.DownloadAction{
								Artifact:          "other bits with checksum",
								From:              "blobstore.com/bits/other-bits-checksum",
								To:                "/usr/local/other",
								CacheKey:          "sha256:some-other-sha256",
								LogSource:         "",
								User:              "legacy-jim",
								ChecksumAlgorithm: "sha256",
								ChecksumValue:     "some-other-sha256",
							},
						),
					)))
				})

				It("converts shared resources into V2 cached dependencies", func() {
					runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					Expect(runReq.CachedDependencies).To(ConsistOf(executor.CachedDependency{
						Name:              "app bits",
						From:              "blobstore.com/bits/app-bits",
						To:                "/usr/local/app",
						CacheKey:          "sha256:some-sha256",
						LogSource:         "",
						ChecksumAlgorithm: "sha256",
						ChecksumValue:     "some-sha256",
					}))
				})

				Context("when the layering mode is 'two-layer'", func() {
					It("converts the rootfs to a preloaded+layer scheme, and first exclusive resource into the extra layer in the rootfs", func() {
						runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeTwoLayer)
						Expect(err).NotTo(HaveOccurred())

						Expect(runReq.RootFSPath).To(Equal(fmt.Sprintf(
							"preloaded+layer:%s?layer=%s&layer_path=%s&layer_digest=%s",
							stackPathMap["cflinuxfs3"],
							url.QueryEscape(task.ImageLayers[1].Url),
							url.QueryEscape(task.ImageLayers[1].DestinationPath),
							url.QueryEscape(task.ImageLayers[1].DigestValue),
						)))
					})

					It("excludes the converted exclusive resource from the setup actions", func() {
						runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeTwoLayer)
						Expect(err).NotTo(HaveOccurred())
						Expect(runReq.Setup).To(BeNil())
					})

					It("converts shared resources into V2 cached dependencies", func() {
						runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeTwoLayer)
						Expect(err).NotTo(HaveOccurred())
						Expect(runReq.CachedDependencies).To(ConsistOf(executor.CachedDependency{
							Name:              "app bits",
							From:              "blobstore.com/bits/app-bits",
							To:                "/usr/local/app",
							CacheKey:          "sha256:some-sha256",
							LogSource:         "",
							ChecksumAlgorithm: "sha256",
							ChecksumValue:     "some-sha256",
						}))
					})
				})
			})

			Describe("the execution backend", func() {
				runRequest := func() executor.RunRequest {
					runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
					Expect(err).NotTo(HaveOccurred())
					return runReq
				}

				Context("when the CF_EXECUTION_BACKEND environment variable is set", func() {
					BeforeEach(func() {
						task.EnvironmentVariables = append(task.EnvironmentVariables,
							&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "vm-qemu"})
					})

					It("sets the execution backend on the run info", func() {
						Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("vm-qemu"))
					})
				})

				Context("when the CF_EXECUTION_BACKEND environment variable is set more than once", func() {
					BeforeEach(func() {
						task.EnvironmentVariables = append(task.EnvironmentVariables,
							&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "gvisor"},
							&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: "vm-qemu"})
					})

					It("uses the last value", func() {
						Expect(runRequest().RunInfo.ExecutionBackend).To(Equal("vm-qemu"))
					})
				})

				Context("when the CF_EXECUTION_BACKEND environment variable is absent", func() {
					It("leaves the execution backend empty", func() {
						Expect(runRequest().RunInfo.ExecutionBackend).To(BeEmpty())
					})
				})

				Describe("the rootfs of a VM backend", func() {
					DescribeTable("which rootfs a backend accepts",
						func(backendName, rootFS string, rejected bool) {
							task.RootFs = rootFS
							task.EnvironmentVariables = append(task.EnvironmentVariables,
								&models.EnvironmentVariable{Name: "CF_EXECUTION_BACKEND", Value: backendName})

							runReq, err := runRequestConversionHelper.NewRunRequestFromTask(task, stackPathMap, rep.LayeringModeSingleLayer)
							if rejected {
								Expect(err).To(MatchError(rep.ErrVMBackendRootFS))
								Expect(err).To(MatchError(ContainSubstring(fmt.Sprintf("backend %q", backendName))))
							} else {
								Expect(err).NotTo(HaveOccurred())
								Expect(runReq.RunInfo.ExecutionBackend).To(Equal(backendName))
							}
						},
						Entry("vm-qemu with a preloaded stack", "vm-qemu", "preloaded:cflinuxfs3", false),
						Entry("vm-firecracker with a preloaded stack and a layer", "vm-firecracker", "preloaded+layer:cflinuxfs3?layer=http://file-server/layer.tgz&layer_digest=some-digest&layer_path=/home/vcap", false),
						Entry("vm-qemu with a docker image", "vm-qemu", "docker:///cloudfoundry/test", false),
						Entry("vm-firecracker with a docker image", "vm-firecracker", "docker:///cloudfoundry/test", false),
						Entry("vm-qemu with an unknown scheme", "vm-qemu", "oci:///some/image", true),
						Entry("vm-qemu with no rootfs", "vm-qemu", "", true),
						Entry("oci-gvisor with a docker image", "oci-gvisor", "docker:///cloudfoundry/test", false),
						Entry("no backend with a docker image", "", "docker:///cloudfoundry/test", false),
					)
				})
			})
		})
	})

	Describe("ConvertPreloadedRootFS", func() {
		var imageLayers []*models.ImageLayer
		BeforeEach(func() {
			imageLayers = []*models.ImageLayer{
				{
					Name:            "bpal-tgz",
					Url:             "http://file-server.service.cf.internal:8080/v1/static/buildpack_app_lifecycle/buildpack_app_lifecycle.tgz",
					DestinationPath: "/tmp/lifecycle",
					LayerType:       models.LayerTypeShared,
					MediaType:       models.MediaTypeTgz,
				},
				{
					Name:            "bpal-tar",
					Url:             "http://file-server.internal/buildpack_app_lifecycle/b.tar",
					DestinationPath: "/tmp/lifecycle2",
					DigestAlgorithm: models.DigestAlgorithmSha512,
					DigestValue:     "long-digest",
					LayerType:       models.LayerTypeExclusive,
					MediaType:       models.MediaTypeTar,
				},
				{
					Name:            "droplet",
					Url:             "https://droplet.com/download",
					DigestAlgorithm: models.DigestAlgorithmSha256,
					DigestValue:     "the-real-digest",
					DestinationPath: "/home/vcap",
					LayerType:       models.LayerTypeExclusive,
					MediaType:       models.MediaTypeTgz,
				},
				{
					Name:            "ruby_buildpack",
					Url:             "https://blobstore.internal/ruby_buildpack.zip",
					DestinationPath: "/tmp/buildpacks/ruby_buildpack",
					DigestAlgorithm: models.DigestAlgorithmSha256,
					DigestValue:     "ruby-buildpack-digest",
					LayerType:       models.LayerTypeExclusive,
					MediaType:       models.MediaTypeZip,
				},
			}
		})

		Context("when the layering mode is single-layer", func() {
			var layeringMode string
			BeforeEach(func() {
				layeringMode = rep.LayeringModeSingleLayer
			})

			It("doesn't convert preloaded+layer rootfs URLs ", func() {
				rootFS := "preloaded+layer:cflinuxfs3?layer=https://blobstore.internal/layer1.tgz?layer_path=/tmp/asset1&layer_digest=asdlfkjsf"
				newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
				Expect(newRootFS).To(Equal(rootFS))
				Expect(newImageLayers).To(Equal(imageLayers))
			})

			It("doesn't convert docker rootfs URLs", func() {
				rootFS := "docker:///cloudfoundry/grace"
				newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
				Expect(newRootFS).To(Equal(rootFS))
				Expect(newImageLayers).To(Equal(imageLayers))
			})

			It("doesn't convert preloaded rootfs URLs", func() {
				rootFS := "preloaded:cflinuxfs3"
				newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
				Expect(newRootFS).To(Equal(rootFS))
				Expect(newImageLayers).To(Equal(imageLayers))
			})
		})

		Context("when the layering mode is two-layer", func() {
			var layeringMode string
			BeforeEach(func() {
				layeringMode = rep.LayeringModeTwoLayer
			})

			It("doesn't convert preloaded+layer rootfs URLs ", func() {
				rootFS := "preloaded+layer:cflinuxfs3?layer=https://blobstore.internal/layer1.tgz?layer_path=/tmp/asset1&layer_digest=asdlfkjsf"
				newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
				Expect(newRootFS).To(Equal(rootFS))
				Expect(newImageLayers).To(Equal(imageLayers))
			})

			It("doesn't convert docker rootfs URLs", func() {
				rootFS := "docker:///cloudfoundry/grace"
				newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
				Expect(newRootFS).To(Equal(rootFS))
				Expect(newImageLayers).To(Equal(imageLayers))
			})

			Context("when the rootfs scheme is 'preloaded'", func() {
				var rootFS string
				BeforeEach(func() {
					rootFS = "preloaded:cflinuxfs3"
				})

				It("converts the rootfs URL scheme to 'preloaded+layer' and converts the first exclusive sha256 tgz layer to the extra layer", func() {
					newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
					expectedRootFS := fmt.Sprintf("preloaded+layer:cflinuxfs3?layer=%s&layer_path=%s&layer_digest=the-real-digest", url.QueryEscape("https://droplet.com/download"), url.QueryEscape("/home/vcap"))
					expectedLayers := []*models.ImageLayer{
						imageLayers[0],
						imageLayers[1],
						imageLayers[3],
					}
					Expect(newRootFS).To(Equal(expectedRootFS))
					Expect(newImageLayers).To(Equal(expectedLayers))
				})

				Context("when the image layers are only shared image types", func() {
					BeforeEach(func() {
						imageLayers[1].LayerType = models.LayerTypeShared
						imageLayers[2].LayerType = models.LayerTypeShared
						imageLayers[3].LayerType = models.LayerTypeShared
					})

					It("doesn't convert rootfs URLs", func() {
						newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
						Expect(newRootFS).To(Equal(rootFS))
						Expect(newImageLayers).To(Equal(imageLayers))
					})
				})

				Context("when there is no layer with a tgz media type", func() {
					BeforeEach(func() {
						imageLayers[2].MediaType = models.MediaTypeZip
					})

					It("doesn't convert rootfs URLs", func() {
						newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
						Expect(newRootFS).To(Equal(rootFS))
						Expect(newImageLayers).To(Equal(imageLayers))
					})
				})
				Context("when there is no layer with a sha256 digest algorithm", func() {
					BeforeEach(func() {
						imageLayers[2].DigestAlgorithm = models.DigestAlgorithmInvalid
					})

					It("doesn't convert rootfs URLs", func() {
						newRootFS, newImageLayers := rep.ConvertPreloadedRootFS(rootFS, imageLayers, layeringMode)
						Expect(newRootFS).To(Equal(rootFS))
						Expect(newImageLayers).To(Equal(imageLayers))
					})
				})
			})
		})
	})
})
