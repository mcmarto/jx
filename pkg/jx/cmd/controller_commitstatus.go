package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jenkins-x/jx/pkg/gits"

	"github.com/jenkins-x/jx/pkg/prow"

	"k8s.io/client-go/kubernetes"

	"github.com/jenkins-x/jx/pkg/extensions"

	"github.com/pkg/errors"

	"github.com/jenkins-x/jx/pkg/builds"

	corev1 "k8s.io/api/core/v1"

	jenkinsv1client "github.com/jenkins-x/jx/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"k8s.io/client-go/tools/cache"

	"github.com/jenkins-x/jx/pkg/log"

	jenkinsv1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"

	"github.com/jenkins-x/jx/pkg/kube"

	"github.com/spf13/cobra"
	"gopkg.in/AlecAivazis/survey.v1/terminal"
)

// ControllerCommitStatusOptions the options for the controller
type ControllerCommitStatusOptions struct {
	ControllerOptions
}

// NewCmdControllerCommitStatus creates a command object for the "create" command
func NewCmdControllerCommitStatus(f Factory, in terminal.FileReader, out terminal.FileWriter, errOut io.Writer) *cobra.Command {
	options := &ControllerCommitStatusOptions{
		ControllerOptions: ControllerOptions{
			CommonOptions: CommonOptions{
				Factory: f,
				In:      in,
				Out:     out,
				Err:     errOut,
			},
		},
	}

	cmd := &cobra.Command{
		Use:   "commitstatus",
		Short: "Updates commit status",
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			CheckErr(err)
		},
	}
	cmd.Flags().BoolVarP(&options.Verbose, "verbose", "v", false, "Enable verbose logging")
	return cmd
}

// Run implements this command
func (o *ControllerCommitStatusOptions) Run() error {
	// Always run in batch mode as a controller is never run interactively
	o.BatchMode = true

	jxClient, ns, err := o.JXClientAndDevNamespace()
	if err != nil {
		return err
	}
	kubeClient, _, err := o.KubeClientAndDevNamespace()
	if err != nil {
		return err
	}
	apisClient, err := o.CreateApiExtensionsClient()
	if err != nil {
		return err
	}
	err = kube.RegisterCommitStatusCRD(apisClient)
	if err != nil {
		return err
	}
	err = kube.RegisterPipelineActivityCRD(apisClient)
	if err != nil {
		return err
	}

	commitstatusListWatch := cache.NewListWatchFromClient(jxClient.JenkinsV1().RESTClient(), "commitstatuses", ns, fields.Everything())
	kube.SortListWatchByName(commitstatusListWatch)
	_, commitstatusController := cache.NewInformer(
		commitstatusListWatch,
		&jenkinsv1.CommitStatus{},
		time.Minute*10,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				o.onCommitStatusObj(obj, jxClient, ns)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				o.onCommitStatusObj(newObj, jxClient, ns)
			},
			DeleteFunc: func(obj interface{}) {

			},
		},
	)
	stop := make(chan struct{})
	go commitstatusController.Run(stop)

	podListWatch := cache.NewListWatchFromClient(kubeClient.CoreV1().RESTClient(), "pods", ns, fields.Everything())
	kube.SortListWatchByName(podListWatch)
	_, podWatch := cache.NewInformer(
		podListWatch,
		&corev1.Pod{},
		time.Minute*10,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				o.onPodObj(obj, jxClient, kubeClient, ns)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				o.onPodObj(newObj, jxClient, kubeClient, ns)
			},
			DeleteFunc: func(obj interface{}) {

			},
		},
	)
	stop = make(chan struct{})
	podWatch.Run(stop)

	if err != nil {
		return err
	}
	return nil
}

func (o *ControllerCommitStatusOptions) onCommitStatusObj(obj interface{}, jxClient jenkinsv1client.Interface, ns string) {
	check, ok := obj.(*jenkinsv1.CommitStatus)
	if !ok {
		log.Fatalf("commit status controller: unexpected type %v\n", obj)
	} else {
		err := o.onCommitStatus(check, jxClient, ns)
		if err != nil {
			log.Fatalf("commit status controller: %v\n", err)
		}
	}
}

func (o *ControllerCommitStatusOptions) onCommitStatus(check *jenkinsv1.CommitStatus, jxClient jenkinsv1client.Interface, ns string) error {
	for _, v := range check.Spec.Items {
		err := o.update(&v, jxClient, ns)
		if err != nil {
			gitProvider, gitRepoInfo, err1 := o.getGitProvider(v.Commit.GitURL)
			if err1 != nil {
				return err1
			}
			_, err1 = extensions.NotifyCommitStatus(v.Commit, "error", "", "Internal Error performing commit status updates", "", v.Context, gitProvider, gitRepoInfo)
			if err1 != nil {
				return err
			}
			return err
		}
	}
	return nil
}

func (o *ControllerCommitStatusOptions) onPodObj(obj interface{}, jxClient jenkinsv1client.Interface, kubeClient kubernetes.Interface, ns string) {
	check, ok := obj.(*corev1.Pod)
	if !ok {
		log.Fatalf("pod watcher: unexpected type %v\n", obj)
	} else {
		err := o.onPod(check, jxClient, kubeClient, ns)
		if err != nil {
			log.Fatalf("pod watcher: %v\n", err)
		}
	}
}

func (o *ControllerCommitStatusOptions) onPod(pod *corev1.Pod, jxClient jenkinsv1client.Interface, kubeClient kubernetes.Interface, ns string) error {
	if pod != nil {
		labels := pod.Labels
		if labels != nil {
			buildName := labels[builds.LabelBuildName]
			if buildName == "" {
				buildName = labels[builds.LabelOldBuildName]
			}
			if buildName != "" {
				org := ""
				repo := ""
				pullRequest := ""
				pullPullSha := ""
				pullBaseSha := ""
				buildNumber := ""
				sourceUrl := ""
				branch := ""
				for _, initContainer := range pod.Spec.InitContainers {
					for _, e := range initContainer.Env {
						switch e.Name {
						case "REPO_OWNER":
							org = e.Value
						case "REPO_NAME":
							repo = e.Value
						case "PULL_NUMBER":
							pullRequest = fmt.Sprintf("PR-%s", e.Value)
						case "PULL_PULL_SHA":
							pullPullSha = e.Value
						case "PULL_BASE_SHA":
							pullBaseSha = e.Value
						case "JX_BUILD_NUMBER":
							buildNumber = e.Value
						case "SOURCE_URL":
							sourceUrl = e.Value
						case "PULL_BASE_REF":
							branch = e.Value
						}
					}
				}
				if org != "" && repo != "" && buildNumber != "" && (pullBaseSha != "" || pullPullSha != "") {

					sha := pullBaseSha
					if pullRequest == "PR-" {
						pullRequest = ""
					} else {
						sha = pullPullSha
						branch = pullRequest
					}
					if o.Verbose {
						log.Infof("pod watcher: build pod: %s, org: %s, repo: %s, buildNumber: %s, pullBaseSha: %s, pullPullSha: %s, pullRequest: %s, sourceUrl: %s\n", pod.Name, org, repo, buildNumber, pullBaseSha, pullPullSha, pullRequest, sourceUrl)
					}
					if sha == "" {
						log.Warnf("No sha on %s, not upserting commit status\n", pod.Name)
					} else {
						prow := prow.Options{
							KubeClient: kubeClient,
							NS:         ns,
						}
						contexts, err := prow.GetBranchProtectionContexts(org, repo)
						if err != nil {
							return err
						}
						if o.Verbose {
							log.Infof("Using contexts %v\n", contexts)
						}
						for _, ctx := range contexts {
							if pullRequest != "" {
								name := kube.ToValidName(fmt.Sprintf("%s-%s-%s-%s", org, repo, branch, ctx))
								pipelineActName := kube.ToValidName(fmt.Sprintf("%s-%s-%s-%s", org, repo, branch, buildNumber))
								err = o.UpsertCommitStatusCheck(name, pipelineActName, sourceUrl, sha, pullRequest, ctx, jxClient, ns)
								if err != nil {
									return err
								}
							}
						}
					}
				}
			}

		}

	}
	return nil
}

func (o *ControllerCommitStatusOptions) UpsertCommitStatusCheck(name string, pipelineActName string, url string, sha string, pullRequest string, context string, jxClient jenkinsv1client.Interface, ns string) error {
	if name != "" {

		status, err := jxClient.JenkinsV1().CommitStatuses(ns).Get(name, metav1.GetOptions{})
		create := false
		update := false
		actRef := jenkinsv1.ResourceReference{}
		if err != nil {
			create = true
		} else {
			log.Infof("commit status controller: commit status already exists for %s\n", name)
		}
		// Create the activity reference
		act, err := jxClient.JenkinsV1().PipelineActivities(ns).Get(pipelineActName, metav1.GetOptions{})
		if err == nil {
			actRef.Name = act.Name
			actRef.Kind = act.Kind
			actRef.UID = act.UID
			actRef.APIVersion = act.APIVersion
		}

		possibleStatusDetails := make([]int, 0)
		for i, v := range status.Spec.Items {
			if v.Commit.SHA == sha {
				possibleStatusDetails = append(possibleStatusDetails, i)
			}
		}
		statusDetails := jenkinsv1.CommitStatusDetails{}
		if len(possibleStatusDetails) == 1 {
			statusDetails = status.Spec.Items[possibleStatusDetails[0]]
			if statusDetails.PipelineActivity.UID != actRef.UID {
				update = true
			}
		} else if len(possibleStatusDetails) != 0 {
			return fmt.Errorf("More than %d status detail for sha %s, should 1 or 0, found %v", len(possibleStatusDetails), sha, possibleStatusDetails)
		}

		if create || update {
			// This is not the same pipeline activity the status was created for,
			// or there is no existing status, so we make a new one
			statusDetails = jenkinsv1.CommitStatusDetails{
				Checked: false,
				Commit: jenkinsv1.CommitStatusCommitReference{
					GitURL:      url,
					PullRequest: pullRequest,
					SHA:         sha,
				},
				PipelineActivity: actRef,
				Context:          context,
			}
		}
		if create {
			log.Infof("commit status controller: Creating commit status for pipeline activity %s\n", pipelineActName)
			status = &jenkinsv1.CommitStatus{
				ObjectMeta: metav1.ObjectMeta{
					Name: name,
					Labels: map[string]string{
						"lastCommitSha": sha,
					},
				},
				Spec: jenkinsv1.CommitStatusSpec{
					Items: []jenkinsv1.CommitStatusDetails{
						statusDetails,
					},
				},
			}
			_, err := jxClient.JenkinsV1().CommitStatuses(ns).Create(status)
			if err != nil {
				return err
			}

		} else if update {
			status.Spec.Items[possibleStatusDetails[0]] = statusDetails
			log.Infof("commit status controller: Resetting commit status for pipeline activity %s\n", pipelineActName)
			_, err := jxClient.JenkinsV1().CommitStatuses(ns).Update(status)
			if err != nil {
				return err
			}
		}
	} else {
		errors.New("commit status controller: Must supply name")
	}
	return nil
}

func (o *ControllerCommitStatusOptions) update(statusDetails *jenkinsv1.CommitStatusDetails, jxClient jenkinsv1client.Interface, ns string) error {
	gitProvider, gitRepoInfo, err := o.getGitProvider(statusDetails.Commit.GitURL)
	if err != nil {
		return err
	}
	pass := false
	if statusDetails.Checked {
		var commentBuilder strings.Builder
		pass = true
		for _, c := range statusDetails.Items {
			if !c.Pass {
				pass = false
				fmt.Fprintf(&commentBuilder, "%s | %s | %s | TODO | `/test this`\n", c.Name, c.Description, statusDetails.Commit.SHA)
			}
		}
		if pass {
			_, err := extensions.NotifyCommitStatus(statusDetails.Commit, "success", "", "%s completed successfully", "", statusDetails.Context, gitProvider, gitRepoInfo)
			if err != nil {
				return err
			}
		} else {
			comment := fmt.Sprintf(
				"The following commit statusDetails checks **failed**, say `/retest` to rerun them all:\n"+
					"\n"+
					"Name | Description | Commit | Details | Rerun command\n"+
					"--- | --- | --- | --- | --- \n"+
					"%s\n"+
					"<details>\n"+
					"\n"+
					"Instructions for interacting with me using PR comments are available [here](https://git.k8s.io/community/contributors/guide/pull-requests.md).  If you have questions or suggestions related to my behavior, please file an issue against the [kubernetes/test-infra](https://github.com/kubernetes/test-infra/issues/new?title=Prow%%20issue:) repository. I understand the commands that are listed [here](https://go.k8s.io/bot-commands).\n"+
					"</details>", commentBuilder.String())
			_, err := extensions.NotifyCommitStatus(statusDetails.Commit, "failure", "", "Some commit statusDetails checks failed", comment, statusDetails.Context, gitProvider, gitRepoInfo)
			if err != nil {
				return err
			}
		}
	} else {
		_, err = extensions.NotifyCommitStatus(statusDetails.Commit, "pending", "", fmt.Sprintf("Waiting for %s to complete", statusDetails.Context), "", statusDetails.Context, gitProvider, gitRepoInfo)
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *ControllerCommitStatusOptions) getGitProvider(url string) (gits.GitProvider, *gits.GitRepositoryInfo, error) {
	// TODO This is an epic hack to get the git stuff working
	gitInfo, err := gits.ParseGitURL(url)
	if err != nil {
		return nil, nil, err
	}
	authConfigSvc, err := o.CreateGitAuthConfigService()
	if err != nil {
		return nil, nil, err
	}
	gitKind, err := o.GitServerKind(gitInfo)
	if err != nil {
		return nil, nil, err
	}
	for _, server := range authConfigSvc.Config().Servers {
		if server.Kind == gitKind && len(server.Users) >= 1 {
			// Just grab the first user for now
			username := server.Users[0].Username
			apiToken := server.Users[0].ApiToken
			err = os.Setenv("GIT_USERNAME", username)
			if err != nil {
				return nil, nil, err
			}
			err = os.Setenv("GIT_API_TOKEN", apiToken)
			if err != nil {
				return nil, nil, err
			}
			break
		}
	}
	return o.createGitProviderForURLWithoutKind(url)
}
