package variables

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jenkins-x-plugins/jx-gitops/pkg/rootcmd"
	"github.com/jenkins-x-plugins/jx-gitops/pkg/variablefinders"
	jxcore "github.com/jenkins-x/jx-api/v4/pkg/apis/core/v4beta1"
	v1 "github.com/jenkins-x/jx-api/v4/pkg/apis/jenkins.io/v1"
	jxc "github.com/jenkins-x/jx-api/v4/pkg/client/clientset/versioned"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/cli"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/giturl"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/activities"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/jxclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/naming"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/services"
	"github.com/jenkins-x/jx-helpers/v3/pkg/scmhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/stringhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Lazily creates a .jx/variables.sh script with common pipeline environment variables
`)

	cmdExample = templates.Examples(`
		# lazily create the .jx/variables.sh file
		%s variables
	`)
)

// Options the options for the command
type Options struct {
	scmhelpers.Options
	File               string
	RepositoryName     string
	RepositoryURL      string
	ConfigMapName      string
	Namespace          string
	VersionFile        string
	BuildNumber        string
	BuildID            string
	GitCommitUsername  string
	GitCommitUserEmail string
	GitBranch          string
	DashboardURL       string
	Commit             bool
	KubeClient         kubernetes.Interface
	JXClient           jxc.Interface
	Requirements       *jxcore.RequirementsConfig
	ConfigMapData      map[string]string
	entries            map[string]*Entry
	factories          []Factory
}

// Entry a variable entry in the file on load
type Entry struct {
	Name  string
	Value string
	Index int
}

// Factory used to create a variable if its not defined locally
type Factory struct {
	Name     string
	Function func() (string, error)
	Value    string
}

// NewCmdVariables creates a command object for the command
func NewCmdVariables() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "variables",
		Short:   "Lazily creates a .jx/variables.sh script with common pipeline environment variables",
		Long:    cmdLong,
		Example: fmt.Sprintf(cmdExample, rootcmd.BinaryName),
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	o.DiscoverFromGit = true
	cmd.Flags().StringVarP(&o.File, "file", "f", filepath.Join(".jx", "variables.sh"), "the default variables file to lazily create or enrich")
	cmd.Flags().StringVarP(&o.RepositoryName, "repo-name", "n", "release-repo", "the name of the helm chart to release to. If not specified uses JX_CHART_REPOSITORY environment variable")
	cmd.Flags().StringVarP(&o.RepositoryURL, "repo-url", "u", "", "the URL to release to")
	cmd.Flags().StringVarP(&o.GitCommitUsername, "git-user-name", "", "", "the user name to git commit")
	cmd.Flags().StringVarP(&o.GitCommitUserEmail, "git-user-email", "", "", "the user email to git commit")
	cmd.Flags().StringVarP(&o.Namespace, "namespace", "", "", "the namespace to look for the dev Environment. Defaults to the current namespace")
	cmd.Flags().StringVarP(&o.BuildNumber, "build-number", "", "", "the build number to use. If not specified defaults to $BUILD_NUMBER")
	cmd.Flags().StringVarP(&o.ConfigMapName, "configmap", "", "jenkins-x-docker-registry", "the ConfigMap used to load environment variables")
	cmd.Flags().StringVarP(&o.VersionFile, "version-file", "", "", "the file to load the version from if not specified directly or via a $VERSION environment variable. Defaults to VERSION in the current dir")
	cmd.Flags().BoolVarP(&o.Commit, "commit", "", true, "commit variables.sh")
	o.Options.AddFlags(cmd)
	return cmd, o
}

// Run implements the command
func (o *Options) Validate() error {
	err := o.Options.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate scm options")
	}
	if o.VersionFile == "" {
		o.VersionFile = filepath.Join(o.Dir, "VERSION")
	}
	if o.entries == nil {
		o.entries = map[string]*Entry{}
	}
	o.JXClient, o.Namespace, err = jxclient.LazyCreateJXClientAndNamespace(o.JXClient, o.Namespace)
	if err != nil {
		return errors.Wrapf(err, "failed to create jx client")
	}
	o.KubeClient, err = kube.LazyCreateKubeClient(o.KubeClient)
	if err != nil {
		return errors.Wrapf(err, "failed to create kube client")
	}

	if o.GitClient == nil {
		o.GitClient = cli.NewCLIClient("", o.CommandRunner)
	}
	if o.Requirements == nil {
		o.Requirements, err = variablefinders.FindRequirements(o.GitClient, o.JXClient, o.Namespace, o.Dir, o.Owner, o.Repository)
		if err != nil {
			return errors.Wrapf(err, "failed to load requirements")
		}
	}

	if o.ConfigMapData == nil {
		cm, err := o.KubeClient.CoreV1().ConfigMaps(o.Namespace).Get(context.TODO(), o.ConfigMapName, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return errors.Wrapf(err, "failed to load ConfigMap %s in namespace %s", o.ConfigMapName, o.Namespace)
			}
		}
		if o.ConfigMapData == nil {
			o.ConfigMapData = map[string]string{}
		}
		if cm != nil && cm.Data != nil {
			for k, v := range cm.Data {
				name := configMapKeyToEnvVar(k)
				o.ConfigMapData[name] = v
			}
		}
		if o.ConfigMapData["MINK_AS"] == "" {
			o.ConfigMapData["MINK_AS"] = "tekton-bot"
		}
	}

	if o.RepositoryURL == "" {
		registryOrg, err := o.dockerRegistryOrg()
		if err != nil {
			return errors.Wrapf(err, "failed to find container registry org")
		}

		o.RepositoryURL, err = variablefinders.FindRepositoryURL(o.Requirements, registryOrg, o.Repository)
		if err != nil {
			return errors.Wrapf(err, "failed to find chart repository URL")
		}
	}

	if o.Options.Branch == "" || o.Options.Branch == "HEAD" {
		o.Options.Branch, err = o.Options.GetBranch()
		if err != nil {
			return errors.Wrapf(err, "failed to find branch name")
		}
	}

	o.BuildNumber, err = o.GetBuildNumber()
	if err != nil {
		return errors.Wrapf(err, "failed to find build number")
	}

	o.factories = []Factory{
		{
			Name: "APP_NAME",
			Function: func() (string, error) {
				return o.Options.Repository, nil
			},
		},
		{
			Name: "BRANCH_NAME",
			Function: func() (string, error) {
				return o.Options.Branch, err
			},
		},
		{
			Name: "BUILD_NUMBER",
			Function: func() (string, error) {
				return o.BuildNumber, nil
			},
		},
		{
			Name: "DOCKERFILE_PATH",
			Function: func() (string, error) {
				return o.FindDockerfilePath()
			},
		},
		{
			Name: "DOCKER_REGISTRY",
			Function: func() (string, error) {
				return o.dockerRegistry()
			},
		},
		{
			Name: "DOCKER_REGISTRY_ORG",
			Function: func() (string, error) {
				return o.dockerRegistryOrg()
			},
		},
		{
			Name: "DOMAIN",
			Function: func() (string, error) {
				return o.Requirements.Ingress.Domain, nil
			},
		},
		{
			Name: "GIT_BRANCH",
			Function: func() (string, error) {
				return o.GetGitBranch()
			},
		},
		{
			Name: "JENKINS_X_URL",
			Function: func() (string, error) {
				return o.GetJenkinsXURL()
			},
		},
		{
			Name: "JX_CHART_REPOSITORY",
			Function: func() (string, error) {
				registryOrg, err := o.dockerRegistryOrg()
				if err != nil {
					return "", errors.Wrapf(err, "failed to find container registry org")
				}
				return variablefinders.FindRepositoryURL(o.Requirements, registryOrg, o.Options.Repository)
			},
		},
		{
			Name: "MINK_IMAGE",
			Function: func() (string, error) {
				return o.minkImage()
			},
		},
		{
			Name: "NAMESPACE_SUB_DOMAIN",
			Function: func() (string, error) {
				return o.Requirements.Ingress.NamespaceSubDomain, nil
			},
		},
		{
			Name: "PIPELINE_KIND",
			Function: func() (string, error) {
				return variablefinders.FindPipelineKind(o.Branch)
			},
		},
		{
			Name: "PUSH_CONTAINER_REGISTRY",
			Function: func() (string, error) {
				return o.pushContainerRegistry()
			},
		},
		{
			Name: "REPO_NAME",
			Function: func() (string, error) {
				return o.getRepoName(), nil
			},
		},
		{
			Name: "REPO_OWNER",
			Function: func() (string, error) {
				return o.Options.Owner, nil
			},
		},
		{
			Name: "VERSION",
			Function: func() (string, error) {
				return variablefinders.FindVersion(o.VersionFile, o.Options.Branch, o.BuildNumber)
			},
		},
	}

	// lets add any extra values from the ConfigMap
	for k, v := range o.ConfigMapData {
		found := false
		for i := range o.factories {
			if o.factories[i].Name == k {
				found = true
			}
		}
		if !found {
			o.factories = append(o.factories, Factory{
				Name:  k,
				Value: v,
			})
		}
	}
	sort.Slice(o.factories, func(i, j int) bool {
		return o.factories[i].Name < o.factories[j].Name
	})
	return nil
}

// Run implements the command
func (o *Options) Run() error {
	err := o.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate")
	}

	file := o.File
	if o.Dir != "" {
		file = filepath.Join(o.Dir, file)
	}
	exists, err := files.FileExists(file)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", file)
	}
	source := ""

	if exists {
		data, err := os.ReadFile(file)
		if err != nil {
			return errors.Wrapf(err, "failed to read file %s", file)
		}
		source = string(data)
		lines := strings.Split(source, "\n")
		for i, line := range lines {
			if strings.HasSuffix(line, "export ") {
				text := strings.TrimSpace(line[len("export "):])
				idx := strings.Index(text, "=")
				if idx > 0 {
					name := strings.TrimSpace(text[0:idx])
					if name != "" {
						value := strings.TrimSpace(text[idx+1:])

						entry := &Entry{
							Name:  name,
							Value: value,
							Index: i,
						}
						o.entries[name] = entry
					}
				}
			}
		}

		source += "\n\n"
	}

	buf := strings.Builder{}
	buf.WriteString("\n# generated by: jx gitops variables\n")

	for i := range o.factories {
		f := &o.factories[i]
		name := f.Name
		entry := o.entries[name]
		value := ""
		if entry != nil {
			value = entry.Value
		}

		if value == "" {
			if f.Function == nil {
				value = f.Value
			} else {
				value, err = f.Function()
				if err != nil {
					return errors.Wrapf(err, "failed to evaluate function for variable %s", name)
				}
			}
			if value != "" {
				log.Logger().Infof("export %s='%s'", name, value)

				line := fmt.Sprintf("export %s='%s'", name, value)
				buf.WriteString(line)
				buf.WriteString("\n")
			}
		}
	}
	if source != "" {
		buf.WriteString("\n\n# content from git...\n")
		buf.WriteString(source)
	}

	source = buf.String()
	dir := filepath.Dir(file)
	err = os.MkdirAll(dir, files.DefaultDirWritePermissions)
	if err != nil {
		return errors.Wrapf(err, "failed to create dir %s", dir)
	}
	err = os.WriteFile(file, []byte(source), files.DefaultFileWritePermissions)
	if err != nil {
		return errors.Wrapf(err, "failed to save %s", file)
	}
	log.Logger().Infof("added variables to file: %s", info(file))

	if o.Commit {
		_, _, err = gitclient.EnsureUserAndEmailSetup(o.GitClient, o.Dir, o.GitCommitUsername, o.GitCommitUserEmail)
		if err != nil {
			return errors.Wrapf(err, "failed to setup git user and email")
		}

		_, err = gitclient.AddAndCommitFiles(o.GitClient, o.Dir, "chore: add variables")
		if err != nil {
			return errors.Wrapf(err, "failed to commit changes")
		}
	}
	return nil
}

func (o *Options) dockerRegistry() (string, error) {
	answer := ""
	if o.Requirements != nil {
		answer = o.Requirements.Cluster.Registry
	}
	if answer == "" {
		answer = o.ConfigMapData["DOCKER_REGISTRY"]
	}
	return strings.ToLower(answer), nil
}

func (o *Options) pushContainerRegistry() (string, error) {
	answer := o.ConfigMapData["PUSH_CONTAINER_REGISTRY"]
	if answer == "" {
		return o.dockerRegistry()
	}
	return answer, nil
}

func (o *Options) dockerRegistryOrg() (string, error) {
	answer := ""
	if answer == "" {
		answer = o.ConfigMapData["DOCKER_REGISTRY_ORG"]
	}
	if answer != "" {
		return answer, nil
	}
	return variablefinders.DockerRegistryOrg(o.Requirements, o.Options.Owner)
}

func (o *Options) GetGitBranch() (string, error) {
	if o.GitBranch == "" {
		var err error
		o.GitBranch, err = gitclient.Branch(o.GitClient, o.Dir)
		if err != nil {
			log.Logger().Warnf("failed to get the current git branch as probably not in a git clone directory, so cannot create the GIT_BRANCH. (%s)", err.Error())
			o.GitBranch = ""
		}
	}
	return o.GitBranch, nil
}

func (o *Options) minkImage() (string, error) {
	registry, err := o.dockerRegistry()
	if err != nil {
		return "", errors.Wrapf(err, "failed to get the docker registry")
	}

	registryOrg, err := o.dockerRegistryOrg()
	if err != nil {
		return "", errors.Wrapf(err, "failed to get the docker registry")
	}

	version, err := variablefinders.FindVersion(o.VersionFile, o.Options.Branch, o.BuildNumber)
	if err != nil {
		return "", errors.Wrapf(err, "failed to find version")
	}

	image := strings.Replace(o.Options.Repository, "/", "-", -1) + ":" + version
	return stringhelpers.UrlJoin(registry, registryOrg, image), nil
}

// GetBuildNumber returns the build number from BUILD_NUMBER or uses PipelineActivities to create/find it
func (o *Options) GetBuildNumber() (string, error) {
	if o.BuildNumber == "" {
		o.BuildNumber = os.Getenv("BUILD_NUMBER")
		if o.BuildNumber == "" {
			var err error
			buildID := o.GetBuildID()
			if buildID != "" {
				o.BuildNumber, err = o.FindBuildNumber(buildID)
				if err != nil {
					return "", errors.Wrapf(err, "failed to find BuildNumber")
				}
			} else {
				log.Logger().Warnf("no $BUILD_ID found so cannot create the BUILD_NUMBER")
			}
		}
	}
	return o.BuildNumber, nil
}

// FindBuildNumber finds the build number for the given build ID
func (o *Options) FindBuildNumber(buildID string) (string, error) {
	// lets try find a PipelineActivity with this build ID...
	activityInterface := o.JXClient.JenkinsV1().PipelineActivities(o.Namespace)

	owner := o.Options.Owner
	repository := o.Options.Repository
	branch := o.Options.Branch
	var activitySlice []*v1.PipelineActivity

	safeOwner := naming.ToValidName(owner)
	safeRepository := naming.ToValidName(repository)
	safeBranch := naming.ToValidName(branch)

	resources, err := activityInterface.List(context.TODO(), metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", errors.Wrapf(err, "failed to find PipelineActivity resources in namespace %s", o.Namespace)
	}
	if resources != nil {
		for i := range resources.Items {
			pa := &resources.Items[i]
			ps := &pa.Spec
			if (ps.GitOwner == owner || ps.GitOwner == safeOwner) &&
				(ps.GitRepository == repository || ps.GitRepository == safeRepository) &&
				(ps.GitBranch == branch || ps.GitBranch == safeBranch) {
				activitySlice = append(activitySlice, pa)
			}
		}
	}

	maxBuild := 0
	for _, pa := range activitySlice {
		labels := pa.Labels
		if labels == nil {
			continue
		}
		if labels["buildID"] == buildID || labels["lighthouse.jenkins-x.io/buildNum"] == buildID {
			if pa.Spec.Build == "" {
				log.Logger().Warnf("PipelineActivity %s does not have a spec.build value", pa.Name)
			} else {
				return pa.Spec.Build, nil
			}
			continue
		}
		if pa.Spec.Build != "" {
			i, err := strconv.Atoi(pa.Spec.Build)
			if err != nil {
				log.Logger().Warnf("PipelineActivity %s has an invalid spec.build number %s should be an integer: %s", pa.Name, pa.Spec.Build, err.Error())
			} else if i > maxBuild {
				maxBuild = i
			}
		}
	}
	o.BuildNumber = strconv.Itoa(maxBuild + 1)

	// lets lazy create a new PipelineActivity for this new build number...
	pipeline := fmt.Sprintf("%s/%s/%s", owner, repository, branch)
	name := naming.ToValidName(pipeline + "-" + o.BuildNumber)

	key := &activities.PromoteStepActivityKey{
		PipelineActivityKey: activities.PipelineActivityKey{
			Name:     name,
			Pipeline: pipeline,
			Build:    o.BuildNumber,
			GitInfo: &giturl.GitRepository{
				Name:         repository,
				Organisation: owner,
			},
			Labels: map[string]string{
				"buildID": buildID,
			},
		},
	}
	_, _, err = key.GetOrCreate(o.JXClient, o.Namespace)
	if err != nil {
		return o.BuildNumber, errors.Wrapf(err, "failed to lazily create PipelineActivity %s", name)
	}
	return o.BuildNumber, nil
}

// GetBuildID returns the current build ID
func (o *Options) GetBuildID() string {
	if o.BuildID == "" {
		o.BuildID = os.Getenv("BUILD_ID")
	}
	return o.BuildID
}

// FindDockerfilePath finds the dockerfile path to use relative to the current directory
func (o *Options) FindDockerfilePath() (string, error) {
	kind, err := variablefinders.FindPipelineKind(o.Branch)
	if err != nil {
		return "", errors.Wrapf(err, "failed to find pipeline kind")
	}
	if kind == "pullrequest" {
		name := "Dockerfile-preview"
		path := filepath.Join(o.Dir, name)
		exists, err := files.FileExists(path)
		if err != nil {
			return "", errors.Wrapf(err, "failed to detect file %s", path)
		}
		if exists {
			return name, nil
		}
	}
	return "Dockerfile", nil
}

// GetJenkinsXURL returns the Jenkins URL
func (o *Options) GetJenkinsXURL() (string, error) {
	dash, err := o.GetDashboardURL()
	if dash == "" || err != nil {
		return "", err
	}
	owner := o.Options.Owner
	repo := strings.Replace(o.Options.Repository, "/", "-", -1)
	branch := o.Options.Branch
	build := o.BuildNumber

	if owner == "" || repo == "" || branch == "" || build == "" {
		return "", nil
	}
	return stringhelpers.UrlJoin(dash, owner, repo, branch, build), nil
}

// GetDashboardURL
func (o *Options) GetDashboardURL() (string, error) {
	if o.DashboardURL == "" {
		var err error
		name := "jx-pipelines-visualizer"
		o.DashboardURL, err = services.GetServiceURLFromName(o.KubeClient, name, o.Namespace)
		if err != nil {
			log.Logger().Warnf("cannot discover the URL of service %s in namespace %s due to %v", name, o.Namespace, err)
		}
	}
	return o.DashboardURL, nil
}

// ToLower is required because repos with capitals in their names are not allowed in chartmuseum and it will throw a 500 error.
func (o *Options) getRepoName() string {
	return strings.Replace(strings.ToLower(o.Options.Repository), "/", "-", -1)
}

func configMapKeyToEnvVar(k string) string {
	text := strings.ToUpper(k)
	text = strings.ReplaceAll(text, ".", "_")
	text = strings.ReplaceAll(text, "-", "_")
	return text
}
