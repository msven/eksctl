package create

import (
	"fmt"
	"strings"

	"github.com/weaveworks/eksctl/pkg/kops"
	"github.com/weaveworks/eksctl/pkg/utils"

	"github.com/weaveworks/eksctl/pkg/actions/addon"

	"github.com/kris-nova/logger"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/tools/clientcmd"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/authconfigmap"
	"github.com/weaveworks/eksctl/pkg/ctl/cmdutils"
	"github.com/weaveworks/eksctl/pkg/ctl/cmdutils/filter"
	"github.com/weaveworks/eksctl/pkg/eks"
	"github.com/weaveworks/eksctl/pkg/gitops"
	"github.com/weaveworks/eksctl/pkg/printers"
	"github.com/weaveworks/eksctl/pkg/utils/kubeconfig"
	"github.com/weaveworks/eksctl/pkg/utils/kubectl"
	"github.com/weaveworks/eksctl/pkg/utils/names"
	"github.com/weaveworks/eksctl/pkg/utils/tasks"
	"github.com/weaveworks/eksctl/pkg/vpc"
)

func createClusterCmd(cmd *cmdutils.Cmd) {
	createClusterCmdWithRunFunc(cmd, func(cmd *cmdutils.Cmd, ngFilter *filter.NodeGroupFilter, params *cmdutils.CreateClusterCmdParams) error {
		return doCreateCluster(cmd, ngFilter, params)
	})
}

func createClusterCmdWithRunFunc(cmd *cmdutils.Cmd, runFunc func(cmd *cmdutils.Cmd, ngFilter *filter.NodeGroupFilter, params *cmdutils.CreateClusterCmdParams) error) {
	cfg := api.NewClusterConfig()
	ng := api.NewNodeGroup()
	cmd.ClusterConfig = cfg

	params := &cmdutils.CreateClusterCmdParams{}

	cmd.SetDescription("cluster", "Create a cluster", "")

	cmd.CobraCommand.RunE = func(_ *cobra.Command, args []string) error {
		cmd.NameArg = cmdutils.GetNameArg(args)
		ngFilter := filter.NewNodeGroupFilter()
		if err := cmdutils.NewCreateClusterLoader(cmd, ngFilter, ng, params).Load(); err != nil {
			return err
		}
		return runFunc(cmd, ngFilter, params)
	}

	exampleClusterName := names.ForCluster("", "")
	exampleNodeGroupName := names.ForNodeGroup("", "")

	cmd.FlagSetGroup.InFlagSet("General", func(fs *pflag.FlagSet) {
		fs.StringVarP(&cfg.Metadata.Name, "name", "n", "", fmt.Sprintf("EKS cluster name (generated if unspecified, e.g. %q)", exampleClusterName))
		cmdutils.AddStringToStringVarPFlag(fs, &cfg.Metadata.Tags, "tags", "", map[string]string{}, "Used to tag the AWS resources")
		cmdutils.AddRegionFlag(fs, &cmd.ProviderConfig)
		fs.BoolVar(cfg.IAM.WithOIDC, "with-oidc", false, "Enable the IAM OIDC provider")
		fs.StringSliceVar(&params.AvailabilityZones, "zones", nil, "(auto-select if unspecified)")
		cmdutils.AddVersionFlag(fs, cfg.Metadata, "")
		cmdutils.AddConfigFileFlag(fs, &cmd.ClusterConfigFile)
		cmdutils.AddTimeoutFlag(fs, &cmd.ProviderConfig.WaitTimeout)
		fs.BoolVarP(&params.InstallWindowsVPCController, "install-vpc-controllers", "", false, "Install VPC controller that's required for Windows workloads")
		fs.BoolVarP(&params.Fargate, "fargate", "", false, "Create a Fargate profile scheduling pods in the default and kube-system namespaces onto Fargate")
	})

	cmd.FlagSetGroup.InFlagSet("Initial nodegroup", func(fs *pflag.FlagSet) {
		fs.StringVar(&ng.Name, "nodegroup-name", "", fmt.Sprintf("name of the nodegroup (generated if unspecified, e.g. %q)", exampleNodeGroupName))
		fs.BoolVar(&params.WithoutNodeGroup, "without-nodegroup", false, "if set, initial nodegroup will not be created")
		cmdutils.AddCommonCreateNodeGroupFlags(fs, cmd, ng, &params.CreateManagedNGOptions)
	})

	cmd.FlagSetGroup.InFlagSet("Cluster and nodegroup add-ons", func(fs *pflag.FlagSet) {
		cmdutils.AddCommonCreateNodeGroupAddonsFlags(fs, ng, params.CreateNGOptions)
	})

	cmd.FlagSetGroup.InFlagSet("VPC networking", func(fs *pflag.FlagSet) {
		fs.IPNetVar(&cfg.VPC.CIDR.IPNet, "vpc-cidr", cfg.VPC.CIDR.IPNet, "global CIDR to use for VPC")
		params.Subnets = map[api.SubnetTopology]*[]string{
			api.SubnetTopologyPrivate: fs.StringSlice("vpc-private-subnets", nil, "re-use private subnets of an existing VPC"),
			api.SubnetTopologyPublic:  fs.StringSlice("vpc-public-subnets", nil, "re-use public subnets of an existing VPC"),
		}
		fs.StringVar(&params.KopsClusterNameForVPC, "vpc-from-kops-cluster", "", "re-use VPC from a given kops cluster")
		fs.StringVar(cfg.VPC.NAT.Gateway, "vpc-nat-mode", api.ClusterSingleNAT, "VPC NAT mode, valid options: HighlyAvailable, Single, Disable")
	})

	cmdutils.AddCommonFlagsForAWS(cmd.FlagSetGroup, &cmd.ProviderConfig, true)

	cmd.FlagSetGroup.InFlagSet("Output kubeconfig", func(fs *pflag.FlagSet) {
		cmdutils.AddCommonFlagsForKubeconfig(fs, &params.KubeconfigPath, &params.AuthenticatorRoleARN, &params.SetContext, &params.AutoKubeconfigPath, exampleClusterName)
		fs.BoolVar(&params.WriteKubeconfig, "write-kubeconfig", true, "toggle writing of kubeconfig")
	})
}

func doCreateCluster(cmd *cmdutils.Cmd, ngFilter *filter.NodeGroupFilter, params *cmdutils.CreateClusterCmdParams) error {
	cfg := cmd.ClusterConfig
	meta := cmd.ClusterConfig.Metadata

	printer := printers.NewJSONPrinter()

	ctl, err := cmd.NewCtl()
	if err != nil {
		return err
	}
	cmdutils.LogRegionAndVersionInfo(meta)

	if cfg.Metadata.Version == "" || cfg.Metadata.Version == "auto" {
		cfg.Metadata.Version = api.DefaultVersion
	}
	if cfg.Metadata.Version != api.DefaultVersion {
		if !api.IsSupportedVersion(cfg.Metadata.Version) {
			if api.IsDeprecatedVersion(cfg.Metadata.Version) {
				return fmt.Errorf("invalid version, %s is no longer supported, supported values: %s\nsee also: https://docs.aws.amazon.com/eks/latest/userguide/kubernetes-versions.html", cfg.Metadata.Version, strings.Join(api.SupportedVersions(), ", "))
			}
			return fmt.Errorf("invalid version, supported values: %s", strings.Join(api.SupportedVersions(), ", "))
		}
	}

	if err := cfg.ValidatePrivateCluster(); err != nil {
		return err
	}

	if err := cfg.ValidateClusterEndpointConfig(); err != nil {
		return err
	}

	// if it's a private only cluster warn the user
	if api.PrivateOnly(cfg.VPC.ClusterEndpoints) {
		logger.Warning(api.ErrClusterEndpointPrivateOnly.Error())
	}

	// if using a custom shared node security group, warn that the rules are managed by default
	if cfg.VPC.SharedNodeSecurityGroup != "" && api.IsEnabled(cfg.VPC.ManageSharedNodeSecurityGroupRules) {
		logger.Warning("security group rules may be added by eksctl; see vpc.manageSharedNodeSecurityGroupRules to disable this behavior")
	}

	if params.AutoKubeconfigPath {
		if params.KubeconfigPath != kubeconfig.DefaultPath() {
			return fmt.Errorf("--kubeconfig and --auto-kubeconfig %s", cmdutils.IncompatibleFlags)
		}
		params.KubeconfigPath = kubeconfig.AutoPath(meta.Name)
	}

	if checkSubnetsGivenAsFlags(params) {
		// undo defaulting and reset it, as it's not set via config file;
		// default value here causes errors as vpc.ImportVPC doesn't
		// treat remote state as authority over local state
		cfg.VPC.CIDR = nil
		// load subnets from local map created from flags, into the config
		for topology := range params.Subnets {
			if err := vpc.ImportSubnetsFromIDList(ctl.Provider.EC2(), cfg, topology, *params.Subnets[topology]); err != nil {
				return err
			}
		}
	}
	logFiltered := cmdutils.ApplyFilter(cfg, ngFilter)
	kubeNodeGroups := cmdutils.ToKubeNodeGroups(cfg)

	if err := eks.ValidateFeatureCompatibility(cfg, kubeNodeGroups); err != nil {
		return err
	}
	if params.InstallWindowsVPCController {
		if !eks.SupportsWindowsWorkloads(kubeNodeGroups) {
			return errors.New("running Windows workloads requires having both Windows and Linux (AmazonLinux2) node groups")
		}
	} else {
		eks.LogWindowsCompatibility(kubeNodeGroups, cfg.Metadata)
	}

	if err := createOrImportVPC(cmd, cfg, params, ctl); err != nil {
		return err
	}

	nodeGroupService := eks.NewNodeGroupService(cfg, ctl.Provider)
	if err := nodeGroupService.Normalize(cmdutils.ToNodePools(cfg)); err != nil {
		return err
	}

	logger.Info("using Kubernetes version %s", meta.Version)
	logger.Info("creating %s", cfg.LogString())

	// TODO dry-run mode should provide a way to render config with all defaults set
	// we should also make a call to resolve the AMI and write the result, similarly
	// the body of the SSH key can be read

	if err := printer.LogObj(logger.Debug, "cfg.json = \\\n%s\n", cfg); err != nil {
		return err
	}

	{ // core action
		stackManager := ctl.NewStackManager(cfg)
		if cmd.ClusterConfigFile == "" {
			logMsg := func(resource string) {
				logger.Info("will create 2 separate CloudFormation stacks for cluster itself and the initial %s", resource)
			}
			if len(cfg.NodeGroups) == 1 {
				logMsg("nodegroup")
			} else if len(cfg.ManagedNodeGroups) == 1 {
				logMsg("managed nodegroup")
			}
		} else {
			logMsg := func(resource string, count int) {
				logger.Info("will create a CloudFormation stack for cluster itself and %d %s stack(s)", count, resource)
			}
			logFiltered()

			logMsg("nodegroup", len(cfg.NodeGroups))
			logMsg("managed nodegroup", len(cfg.ManagedNodeGroups))
		}

		logger.Info("if you encounter any issues, check CloudFormation console or try 'eksctl utils describe-stacks --region=%s --cluster=%s'", meta.Region, meta.Name)
		supportsManagedNodes, err := eks.VersionSupportsManagedNodes(cfg.Metadata.Version)
		if err != nil {
			return err
		}
		postClusterCreationTasks := ctl.CreateExtraClusterConfigTasks(cfg, params.InstallWindowsVPCController)

		supported, err := utils.IsMinVersion(api.Version1_18, cfg.Metadata.Version)
		if err != nil {
			return err
		}

		var taskTree *tasks.TaskTree
		if supported {
			createAddonTasks := addon.CreateAddonTasks(cfg, ctl, true)
			createAddonTasks.IsSubTask = true
			taskTree = stackManager.NewTasksToCreateClusterWithNodeGroups(cfg.NodeGroups, cfg.ManagedNodeGroups, supportsManagedNodes, postClusterCreationTasks, createAddonTasks)
		} else {
			taskTree = stackManager.NewTasksToCreateClusterWithNodeGroups(cfg.NodeGroups, cfg.ManagedNodeGroups, supportsManagedNodes, postClusterCreationTasks)
		}

		logger.Info(taskTree.Describe())
		if errs := taskTree.DoAllSync(); len(errs) > 0 {
			logger.Warning("%d error(s) occurred and cluster hasn't been created properly, you may wish to check CloudFormation console", len(errs))
			logger.Info("to cleanup resources, run 'eksctl delete cluster --region=%s --name=%s'", meta.Region, meta.Name)
			for _, err := range errs {
				ufe := &api.UnsupportedFeatureError{}
				if errors.As(err, &ufe) {
					logger.Critical(ufe.Message)
				}
				logger.Critical("%s\n", err.Error())
			}
			return fmt.Errorf("failed to create cluster %q", meta.Name)
		}
	}

	logger.Info("waiting for the control plane availability...")

	// obtain cluster credentials, write kubeconfig

	{ // post-creation action
		var kubeconfigContextName string

		if params.WriteKubeconfig {
			kubectlConfig := kubeconfig.NewForKubectl(cfg, ctl.GetUsername(), params.AuthenticatorRoleARN, ctl.Provider.Profile())
			kubeconfigContextName = kubectlConfig.CurrentContext

			params.KubeconfigPath, err = kubeconfig.Write(params.KubeconfigPath, *kubectlConfig, params.SetContext)
			if err != nil {
				logger.Warning("unable to write kubeconfig %s, please retry with 'eksctl utils write-kubeconfig -n %s': %v", params.KubeconfigPath, meta.Name, err)
			} else {
				logger.Success("saved kubeconfig as %q", params.KubeconfigPath)
			}
		} else {
			params.KubeconfigPath = ""
		}

		ngTasks := ctl.ClusterTasksForNodeGroups(cfg, params.InstallNeuronDevicePlugin, params.InstallNvidiaDevicePlugin)

		logger.Info(ngTasks.Describe())
		if errs := ngTasks.DoAllSync(); len(errs) > 0 {
			logger.Warning("%d error(s) occurred and post actions have failed, you may wish to check CloudFormation console", len(errs))
			logger.Info("to cleanup resources, run 'eksctl delete cluster --region=%s --name=%s'", meta.Region, meta.Name)
			for _, err := range errs {
				logger.Critical("%s\n", err.Error())
			}
			return fmt.Errorf("failed to create cluster %q", meta.Name)
		}
		logger.Success("all EKS cluster resources for %q have been created", meta.Name)

		// create Kubernetes client
		clientSet, err := ctl.NewStdClientSet(cfg)
		if err != nil {
			return err
		}

		for _, ng := range cfg.NodeGroups {
			// authorise nodes to join
			if err = authconfigmap.AddNodeGroup(clientSet, ng); err != nil {
				return err
			}

			// wait for nodes to join
			if err = ctl.WaitForNodes(clientSet, ng); err != nil {
				return err
			}
		}

		for _, ng := range cfg.ManagedNodeGroups {
			if err := ctl.WaitForNodes(clientSet, ng); err != nil {
				return err
			}
		}

		// FLUX V1 DEPRECATION NOTICE. https://github.com/weaveworks/eksctl/issues/2963
		if cfg.HasGitopsRepoConfigured() {
			logger.Warning("git.X configuration is marked for deprecation: Please see https://github.com/weaveworks/eksctl/issues/2963")
		}
		if cfg.HasGitopsRepoConfigured() || cfg.HasGitOpsFluxConfigured() {
			kubernetesClientConfigs, err := ctl.NewClient(cfg)
			if err != nil {
				return err
			}
			k8sConfig := kubernetesClientConfigs.Config
			k8sRestConfig, err := clientcmd.NewDefaultClientConfig(*k8sConfig, &clientcmd.ConfigOverrides{}).ClientConfig()
			if err != nil {
				return errors.Wrap(err, "cannot create Kubernetes client configuration")
			}
			err = gitops.Setup(params.KubeconfigPath, k8sRestConfig, clientSet, cfg, gitops.DefaultPodReadyTimeout)
			if err != nil {
				return err
			}
			return nil
		}

		env, err := ctl.GetCredentialsEnv()
		if err != nil {
			return err
		}
		if err := kubectl.CheckAllCommands(params.KubeconfigPath, params.SetContext, kubeconfigContextName, env); err != nil {
			logger.Critical("%s\n", err.Error())
			logger.Info("cluster should be functional despite missing (or misconfigured) client binaries")
		}

		if cfg.PrivateCluster.Enabled {
			// disable public access
			logger.Info("disabling public endpoint access for the cluster")
			cfg.VPC.ClusterEndpoints.PublicAccess = api.Disabled()
			if err := ctl.UpdateClusterConfigForEndpoints(cfg); err != nil {
				return errors.Wrap(err, "error disabling public endpoint access for the cluster")
			}
			logger.Info("fully private cluster %q has been created. For subsequent operations, eksctl must be run from within the cluster's VPC, a peered VPC or some other means like AWS Direct Connect", cfg.Metadata.Name)
		}
	}

	logger.Success("%s is ready", meta.LogString())

	if err := printer.LogObj(logger.Debug, "cfg.json = \\\n%s\n", cfg); err != nil {
		return err
	}

	return nil
}

func createOrImportVPC(cmd *cmdutils.Cmd, cfg *api.ClusterConfig, params *cmdutils.CreateClusterCmdParams, ctl *eks.ClusterProvider) error {
	customNetworkingNotice := "custom VPC/subnets will be used; if resulting cluster doesn't function as expected, make sure to review the configuration of VPC/subnets"

	subnetsGiven := cfg.HasAnySubnets() // this will be false when neither flags nor config has any subnets
	if !subnetsGiven && params.KopsClusterNameForVPC == "" {
		// default: create dedicated VPC
		if err := ctl.SetAvailabilityZones(cfg, params.AvailabilityZones); err != nil {
			return err
		}
		if err := vpc.SetSubnets(cfg.VPC, cfg.AvailabilityZones); err != nil {
			return err
		}
		return nil
	}

	if params.KopsClusterNameForVPC != "" {
		// import VPC from a given kops cluster
		if len(params.AvailabilityZones) != 0 {
			return fmt.Errorf("--vpc-from-kops-cluster and --zones %s", cmdutils.IncompatibleFlags)
		}
		if cmd.CobraCommand.Flag("vpc-cidr").Changed {
			return fmt.Errorf("--vpc-from-kops-cluster and --vpc-cidr %s", cmdutils.IncompatibleFlags)
		}

		if subnetsGiven {
			return fmt.Errorf("--vpc-from-kops-cluster and --vpc-private-subnets/--vpc-public-subnets %s", cmdutils.IncompatibleFlags)
		}

		kw, err := kops.NewWrapper(cmd.ProviderConfig.Region, params.KopsClusterNameForVPC)
		if err != nil {
			return err
		}

		if err := kw.UseVPC(ctl.Provider.EC2(), cfg); err != nil {
			return err
		}

		if err := cfg.CanUseForPrivateNodeGroups(); err != nil {
			return err
		}

		logger.Success("using %s from kops cluster %q", cfg.SubnetInfo(), params.KopsClusterNameForVPC)
		logger.Warning(customNetworkingNotice)
		return nil
	}

	// use subnets as specified by --vpc-{private,public}-subnets flags

	if len(params.AvailabilityZones) != 0 {
		return fmt.Errorf("--vpc-private-subnets/--vpc-public-subnets and --zones %s", cmdutils.IncompatibleFlags)
	}
	if cmd.CobraCommand.Flag("vpc-cidr").Changed {
		return fmt.Errorf("--vpc-private-subnets/--vpc-public-subnets and --vpc-cidr %s", cmdutils.IncompatibleFlags)
	}

	if err := vpc.ImportSubnetsFromSpec(ctl.Provider, cfg); err != nil {
		return err
	}

	if err := cfg.HasSufficientSubnets(); err != nil {
		logger.Critical("unable to use given %s", cfg.SubnetInfo())
		return err
	}

	if err := cfg.CanUseForPrivateNodeGroups(); err != nil {
		return err
	}

	logger.Success("using existing %s", cfg.SubnetInfo())
	logger.Warning(customNetworkingNotice)
	return nil
}

func checkSubnetsGivenAsFlags(params *cmdutils.CreateClusterCmdParams) bool {
	return len(*params.Subnets[api.SubnetTopologyPrivate])+len(*params.Subnets[api.SubnetTopologyPublic]) != 0
}
