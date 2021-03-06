package integration_test

import (
	"fmt"
	"net/http"
	"os/exec"

	"github.com/concourse/atc"
	"github.com/gorilla/websocket"
	"github.com/mgutz/ansi"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/onsi/gomega/ghttp"
)

var _ = Describe("Hijacking", func() {
	var hijacked <-chan struct{}
	var workingDirectory string
	var envVariables []string
	var user string
	var path string
	var args []string

	BeforeEach(func() {
		hijacked = nil
		workingDirectory = ""
		envVariables = nil
		user = "root"
		path = "bash"
		args = nil
	})

	upgrader := websocket.Upgrader{}

	hijackHandler := func(id string, didHijack chan<- struct{}, errorMessages []string) http.HandlerFunc {
		return ghttp.CombineHandlers(
			ghttp.VerifyRequest("GET", fmt.Sprintf("/api/v1/containers/%s/hijack", id)),
			func(w http.ResponseWriter, r *http.Request) {
				defer GinkgoRecover()

				conn, err := upgrader.Upgrade(w, r, nil)
				Expect(err).NotTo(HaveOccurred())

				defer conn.Close()

				close(didHijack)

				var processSpec atc.HijackProcessSpec
				err = conn.ReadJSON(&processSpec)
				Expect(err).NotTo(HaveOccurred())

				Expect(processSpec.User).To(Equal(user))
				Expect(processSpec.Dir).To(Equal(workingDirectory))
				for _, envVariable := range envVariables {
					Expect(processSpec.Env).To(ContainElement(envVariable))
				}
				Expect(processSpec.Path).To(Equal(path))
				Expect(processSpec.Args).To(Equal(args))

				var payload atc.HijackInput

				err = conn.ReadJSON(&payload)
				Expect(err).NotTo(HaveOccurred())

				Expect(payload).To(Equal(atc.HijackInput{
					Stdin: []byte("some stdin"),
				}))

				err = conn.WriteJSON(atc.HijackOutput{
					Stdout: []byte("some stdout"),
				})
				Expect(err).NotTo(HaveOccurred())

				err = conn.WriteJSON(atc.HijackOutput{
					Stderr: []byte("some stderr"),
				})
				Expect(err).NotTo(HaveOccurred())

				if len(errorMessages) > 0 {
					for _, msg := range errorMessages {
						err := conn.WriteJSON(atc.HijackOutput{
							Error: msg,
						})
						Expect(err).NotTo(HaveOccurred())
					}

					return
				}

				var closePayload atc.HijackInput

				err = conn.ReadJSON(&closePayload)
				Expect(err).NotTo(HaveOccurred())

				Expect(closePayload).To(Equal(atc.HijackInput{
					Closed: true,
				}))

				exitStatus := 123
				err = conn.WriteJSON(atc.HijackOutput{
					ExitStatus: &exitStatus,
				})
				Expect(err).NotTo(HaveOccurred())
			},
		)
	}

	fly := func(command string, args ...string) {
		commandWithArgs := append([]string{command}, args...)

		flyCmd := exec.Command(flyPath, append([]string{"-t", targetName}, commandWithArgs...)...)

		stdin, err := flyCmd.StdinPipe()
		Expect(err).NotTo(HaveOccurred())

		sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())

		Eventually(hijacked).Should(BeClosed())

		_, err = fmt.Fprintf(stdin, "some stdin")
		Expect(err).NotTo(HaveOccurred())

		Eventually(sess.Out).Should(gbytes.Say("some stdout"))
		Eventually(sess.Err).Should(gbytes.Say("some stderr"))

		err = stdin.Close()
		Expect(err).NotTo(HaveOccurred())

		<-sess.Exited
		Expect(sess.ExitCode()).To(Equal(123))
	}

	hijack := func(args ...string) {
		fly("hijack", args...)
	}

	Context("with only a step name specified", func() {
		BeforeEach(func() {
			didHijack := make(chan struct{})
			hijacked = didHijack

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/builds"),
					ghttp.RespondWithJSONEncoded(200, []atc.Build{
						{ID: 4, Name: "1", Status: "started", JobName: "some-job"},
						{ID: 3, Name: "3", Status: "started"},
						{ID: 2, Name: "2", Status: "started"},
						{ID: 1, Name: "1", Status: "finished"},
					}),
				),
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/containers", "build-id=3&step_name=some-step"),
					ghttp.RespondWithJSONEncoded(200, []atc.Container{
						{ID: "container-id-1", BuildID: 3, StepType: "task", StepName: "some-step", User: user},
					}),
				),
				hijackHandler("container-id-1", didHijack, nil),
			)
		})

		It("hijacks the most recent one-off build", func() {
			hijack("-s", "some-step")
		})

		It("hijacks the most recent one-off build with a more politically correct command", func() {
			fly("intercept", "-s", "some-step")
		})
	})

	Context("when the container specifies a working directory", func() {
		BeforeEach(func() {
			didHijack := make(chan struct{})
			hijacked = didHijack
			workingDirectory = "/tmp/build/my-favorite-guid"

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/builds"),
					ghttp.RespondWithJSONEncoded(200, []atc.Build{
						{ID: 3, Name: "3", Status: "started"},
					}),
				),
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/containers", "build-id=3&step_name=some-step"),
					ghttp.RespondWithJSONEncoded(200, []atc.Container{
						{ID: "container-id-1", BuildID: 3, StepType: "task", StepName: "some-step", WorkingDirectory: workingDirectory, User: user},
					}),
				),
				hijackHandler("container-id-1", didHijack, nil),
			)
		})

		It("hijacks the most recent one-off build in the specified working directory", func() {
			hijack("-s", "some-step")
		})
	})

	Context("when the container specifies environment variables", func() {
		BeforeEach(func() {
			didHijack := make(chan struct{})
			hijacked = didHijack
			envVariables = []string{"VAR1=val1", "VAR2=val2"}

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/builds"),
					ghttp.RespondWithJSONEncoded(200, []atc.Build{
						{ID: 3, Name: "3", Status: "started"},
					}),
				),
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/containers", "build-id=3&step_name=some-step"),
					ghttp.RespondWithJSONEncoded(200, []atc.Container{
						{ID: "container-id-1", BuildID: 3, StepType: "task", StepName: "some-step", EnvironmentVariables: envVariables, User: user},
					}),
				),
				hijackHandler("container-id-1", didHijack, nil),
			)
		})

		It("hijacks the most recent one-off build and sets the specified environment variables", func() {
			hijack("-s", "some-step")
		})
	})

	Context("when the container specifies a user", func() {
		BeforeEach(func() {
			didHijack := make(chan struct{})
			hijacked = didHijack
			user = "amelia"

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/builds"),
					ghttp.RespondWithJSONEncoded(200, []atc.Build{
						{ID: 3, Name: "3", Status: "started"},
					}),
				),
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/containers", "build-id=3&step_name=some-step"),
					ghttp.RespondWithJSONEncoded(200, []atc.Container{
						{ID: "container-id-1", BuildID: 3, StepType: "task", StepName: "some-step", User: "amelia"},
					}),
				),
				hijackHandler("container-id-1", didHijack, nil),
			)
		})

		It("hijacks the most recent one-off build as the specified user", func() {
			hijack("-s", "some-step")
		})
	})

	Context("when no containers are found", func() {
		BeforeEach(func() {
			didHijack := make(chan struct{})
			hijacked = didHijack

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/builds"),
					ghttp.RespondWithJSONEncoded(200, []atc.Build{
						{ID: 1, Name: "1", Status: "finished"},
					}),
				),
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/containers", "build-id=1&step_name=some-step"),
					ghttp.RespondWithJSONEncoded(200, []atc.Container{}),
				),
				hijackHandler("container-id-1", didHijack, nil),
			)
		})

		It("return a friendly error message", func() {
			flyCmd := exec.Command(flyPath, "-t", targetName, "hijack", "-s", "some-step")
			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			Eventually(sess).Should(gexec.Exit(1))

			Expect(sess.Err).To(gbytes.Say("no containers matched your search parameters!\n\nthey may have expired if your build hasn't recently finished.\n"))
		})
	})

	Context("when no containers are found", func() {
		BeforeEach(func() {
			didHijack := make(chan struct{})
			hijacked = didHijack
			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/containers", "build-id=0"),
					ghttp.RespondWithJSONEncoded(200, []atc.Container{}),
				),
			)
		})

		It("logs an error message and response status/body", func() {
			flyCmd := exec.Command(flyPath, "-t", targetName, "hijack", "-b", "0")

			stdin, err := flyCmd.StdinPipe()
			Expect(err).NotTo(HaveOccurred())

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			Eventually(sess.Err.Contents).Should(ContainSubstring("no containers matched your search parameters!\n\nthey may have expired if your build hasn't recently finished.\n"))

			err = stdin.Close()
			Expect(err).NotTo(HaveOccurred())

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(1))
		})
	})

	Context("when multiple step containers are found", func() {
		BeforeEach(func() {
			didHijack := make(chan struct{})
			hijacked = didHijack

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/containers", "pipeline_name=pipeline-name-1&job_name=some-job"),
					ghttp.RespondWithJSONEncoded(200, []atc.Container{
						{
							ID:           "container-id-1",
							WorkerName:   "worker-name-1",
							PipelineName: "pipeline-name-1",
							JobName:      "some-job",
							BuildName:    "2",
							BuildID:      12,
							StepType:     "get",
							StepName:     "some-input",
							Attempts:     []int{1, 1, 1},
							User:         user,
						},
						{
							ID:           "container-id-2",
							WorkerName:   "worker-name-2",
							PipelineName: "pipeline-name-1",
							JobName:      "some-job",
							BuildName:    "2",
							BuildID:      13,
							StepType:     "put",
							StepName:     "some-output",
							Attempts:     []int{1, 1, 2},
							User:         user,
						},
						{
							ID:           "container-id-3",
							WorkerName:   "worker-name-2",
							PipelineName: "pipeline-name-2",
							JobName:      "some-job",
							BuildName:    "2",
							BuildID:      13,
							StepName:     "some-output",
							Attempts:     []int{1},
							User:         user,
						},
						{
							ID:           "container-id-4",
							WorkerName:   "worker-name-2",
							PipelineName: "pipeline-name-2",
							ResourceName: "banana",
							User:         user,
						},
					}),
				),
				hijackHandler("container-id-2", didHijack, nil),
			)
		})

		It("asks the user to select the container from a menu", func() {
			flyCmd := exec.Command(flyPath, "-t", targetName, "hijack", "-j", "pipeline-name-1/some-job")

			stdin, err := flyCmd.StdinPipe()
			Expect(err).NotTo(HaveOccurred())

			sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).NotTo(HaveOccurred())

			Eventually(sess.Out).Should(gbytes.Say("1. resource: banana, type: check"))
			Eventually(sess.Out).Should(gbytes.Say("2. build #2, step: some-input, type: get, attempt: 1.1.1"))
			Eventually(sess.Out).Should(gbytes.Say("3. build #2, step: some-output, type: check, attempt: 1"))
			Eventually(sess.Out).Should(gbytes.Say("4. build #2, step: some-output, type: put, attempt: 1.1.2"))
			Eventually(sess.Out).Should(gbytes.Say("choose a container: "))

			_, err = fmt.Fprintf(stdin, "4\n")
			Expect(err).NotTo(HaveOccurred())

			Eventually(hijacked).Should(BeClosed())

			_, err = fmt.Fprintf(stdin, "some stdin")
			Expect(err).NotTo(HaveOccurred())

			Eventually(sess.Out).Should(gbytes.Say("some stdout"))
			Eventually(sess.Err).Should(gbytes.Say("some stderr"))

			err = stdin.Close()
			Expect(err).NotTo(HaveOccurred())

			<-sess.Exited
			Expect(sess.ExitCode()).To(Equal(123))
		})
	})

	Context("when hijack returns a single container", func() {
		var (
			containerArguments string
			stepType           string
			stepName           string
			buildID            int
			hijackHandlerError []string
			pipelineName       string
			resourceName       string
			jobName            string
			buildName          string
			attempt            []int
		)

		BeforeEach(func() {
			hijackHandlerError = nil
			pipelineName = "a-pipeline"
			jobName = ""
			buildName = ""
			buildID = 0
			stepType = ""
			stepName = ""
			resourceName = ""
			containerArguments = ""
			hijackHandlerError = nil
			attempt = []int{}
		})

		JustBeforeEach(func() {
			didHijack := make(chan struct{})
			hijacked = didHijack

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/api/v1/containers", containerArguments),
					ghttp.RespondWithJSONEncoded(200, []atc.Container{
						{ID: "container-id-1", WorkerName: "some-worker", PipelineName: pipelineName, JobName: jobName, BuildName: buildName, BuildID: buildID, StepType: stepType, StepName: stepName, ResourceName: resourceName, Attempts: attempt, User: user},
					}),
				),
				hijackHandler("container-id-1", didHijack, hijackHandlerError),
			)
		})

		Context("when called with check container", func() {
			BeforeEach(func() {
				resourceName = "some-resource-name"
			})

			Context("and with pipeline specified", func() {
				BeforeEach(func() {
					containerArguments = "type=check&resource_name=some-resource-name&pipeline_name=a-pipeline"
				})

				It("can accept the check resources name and a pipeline", func() {
					hijack("--check", "a-pipeline/some-resource-name")
				})
			})
		})

		Context("when called with a specific build id", func() {
			BeforeEach(func() {
				containerArguments = "build-id=2&step_name=some-step"
				stepType = "task"
				stepName = "some-step"
				buildID = 2
			})

			It("hijacks the most recent one-off build", func() {
				hijack("-b", "2", "-s", "some-step")
			})
		})

		Context("when called with a specific job", func() {
			BeforeEach(func() {
				containerArguments = "pipeline_name=some-pipeline&job_name=some-job&step_name=some-step"
				jobName = "some-job"
				buildName = "3"
				buildID = 13
				stepType = "task"
				stepName = "some-step"
			})

			It("hijacks the job's next build", func() {
				hijack("--job", "some-pipeline/some-job", "--step", "some-step")
			})

			Context("with a specific build of the job", func() {
				BeforeEach(func() {
					containerArguments = "pipeline_name=some-pipeline&job_name=some-job&build_name=3&step_name=some-step"
				})

				It("hijacks the given build", func() {
					hijack("--job", "some-pipeline/some-job", "--build", "3", "--step", "some-step")
				})
			})
		})

		Context("when called with a specific attempt number", func() {
			BeforeEach(func() {
				containerArguments = "pipeline_name=some-pipeline&job_name=some-job&step_name=some-step&attempt=[2,4]"
				jobName = "some-job"
				buildName = "3"
				buildID = 13
				stepType = "task"
				stepName = "some-step"
				attempt = []int{2, 4}
			})

			It("hijacks the job's next build", func() {
				hijack("--job", "some-pipeline/some-job", "--step", "some-step", "--attempt", "2", "--attempt", "4")
			})
		})

		Context("when called with a specific path and args", func() {
			BeforeEach(func() {
				path = "sh"
				args = []string{"echo hello"}

				containerArguments = "build-id=2&step_name=some-step"
				stepType = "task"
				stepName = "some-step"
				buildID = 2
			})

			It("hijacks and runs the provided path with args", func() {
				hijack("-b", "2", "-s", "some-step", "sh", "echo hello")
			})
		})

		Context("when hijacking yields an error", func() {
			BeforeEach(func() {
				resourceName = "some-resource-name"
				containerArguments = "type=check&resource_name=some-resource-name&pipeline_name=a-pipeline"
				hijackHandlerError = []string{"something went wrong"}
			})

			It("prints it to stderr and exits 255", func() {
				flyCmd := exec.Command(flyPath, "-t", targetName, "hijack", "--check", "a-pipeline/some-resource-name")

				stdin, err := flyCmd.StdinPipe()
				Expect(err).NotTo(HaveOccurred())

				sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).NotTo(HaveOccurred())

				Eventually(hijacked).Should(BeClosed())

				_, err = fmt.Fprintf(stdin, "some stdin")
				Expect(err).NotTo(HaveOccurred())

				Eventually(sess.Err.Contents).Should(ContainSubstring(ansi.Color("something went wrong", "red+b") + "\n"))

				err = stdin.Close()
				Expect(err).NotTo(HaveOccurred())

				<-sess.Exited
				Expect(sess.ExitCode()).To(Equal(255))
			})
		})
	})
})
