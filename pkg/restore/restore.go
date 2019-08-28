package restore

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/appscode/go/log"
	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/reference"
	"k8s.io/kubernetes/pkg/apis/core"
	api_v1beta1 "stash.appscode.dev/stash/apis/stash/v1beta1"
	cs "stash.appscode.dev/stash/client/clientset/versioned"
	stash_scheme "stash.appscode.dev/stash/client/clientset/versioned/scheme"
	"stash.appscode.dev/stash/pkg/eventer"
	"stash.appscode.dev/stash/pkg/restic"
	"stash.appscode.dev/stash/pkg/status"
	"stash.appscode.dev/stash/pkg/util"
)

const (
	RestoreModelInitContainer = "init-container"
	RestoreModelJob           = "job"
)

type Options struct {
	Config             *rest.Config
	MasterURL          string
	KubeconfigPath     string
	Namespace          string
	RestoreSessionName string
	BackoffMaxWait     time.Duration

	SetupOpt restic.SetupOptions
	Metrics  restic.MetricsOptions

	KubeClient   kubernetes.Interface
	StashClient  cs.Interface
	RestoreModel string
}

func Restore(opt *Options) error {

	// get the RestoreSession crd
	restoreSession, err := opt.StashClient.StashV1beta1().RestoreSessions(opt.Namespace).Get(opt.RestoreSessionName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if restoreSession.Spec.Target == nil {
		return fmt.Errorf("invalid RestoreSession. Target is nil")
	}

	repository, err := opt.StashClient.StashV1alpha1().Repositories(opt.Namespace).Get(restoreSession.Spec.Repository.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	host, err := util.GetHostName(restoreSession.Spec.Target)
	if err != nil {
		return err
	}

	extraOptions := util.ExtraOptions{
		Host:        host,
		SecretDir:   opt.SetupOpt.SecretDir,
		EnableCache: opt.SetupOpt.EnableCache,
		ScratchDir:  opt.SetupOpt.ScratchDir,
	}
	setupOptions, err := util.SetupOptionsForRepository(*repository, extraOptions)
	if err != nil {
		return err
	}
	// apply nice, ionice settings from env
	setupOptions.Nice, err = util.NiceSettingsFromEnv()
	if err != nil {
		return err
	}
	setupOptions.IONice, err = util.IONiceSettingsFromEnv()
	if err != nil {
		return err
	}
	opt.SetupOpt = setupOptions

	// if we are restoring using job then there no need to lock the repository
	if opt.RestoreModel == RestoreModelJob {
		return opt.runRestore(restoreSession)
	} else {
		// only one pod can acquire restic repository lock. so we need leader election to determine who will acquire the lock
		return opt.electRestoreLeader(restoreSession)
	}
}

func (opt *Options) electRestoreLeader(restoreSession *api_v1beta1.RestoreSession) error {

	log.Infoln("Attempting to elect restore leader")

	rlc := resourcelock.ResourceLockConfig{
		Identity:      os.Getenv(util.KeyPodName),
		EventRecorder: eventer.NewEventRecorder(opt.KubeClient, eventer.EventSourceRestoreInitContainer),
	}

	resLock, err := resourcelock.New(
		resourcelock.ConfigMapsResourceLock,
		restoreSession.Namespace,
		util.GetRestoreConfigmapLockName(restoreSession.Spec.Target.Ref),
		opt.KubeClient.CoreV1(),
		opt.KubeClient.CoordinationV1(),
		rlc,
	)
	if err != nil {
		return fmt.Errorf("error during leader election: %s", err)
	}

	// use a Go context so we can tell the leader election code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// start the leader election code loop
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:          resLock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Infoln("Got leadership, preparing for restore")
				// run restore process
				err := opt.runRestore(restoreSession)
				if err != nil {
					e2 := HandleRestoreFailure(opt, err)
					if e2 != nil {
						err = errors.NewAggregate([]error{err, e2})
					}
					// step down from leadership so that other replicas try to restore
					cancel()
					// fail the container so that it restart and re-try to restore
					log.Fatalln("failed to complete restore. Reason: ", err.Error())
				}
				// restore process is complete. now, step down from leadership so that other replicas can start
				cancel()
			},
			OnStoppedLeading: func() {
				log.Infoln("Lost leadership")
			},
		},
	})
	return nil
}

func (opt *Options) runRestore(restoreSession *api_v1beta1.RestoreSession) error {

	host, err := util.GetHostName(restoreSession.Spec.Target)
	if err != nil {
		return err
	}

	// if already restored for this host then don't process further
	if opt.isRestoredForThisHost(restoreSession, host) {
		log.Infof("Skipping restore for RestoreSession %s/%s. Reason: RestoreSession already processed for host %q.", restoreSession.Namespace, restoreSession.Name, host)
		return nil
	}

	// setup restic wrapper
	w, err := restic.NewResticWrapper(opt.SetupOpt)
	if err != nil {
		return err
	}

	// run restore process
	restoreOutput, restoreErr := w.RunRestore(util.RestoreOptionsForHost(host, restoreSession.Spec.Rules))

	// Update Backup Session and Repository status
	o := status.UpdateStatusOptions{
		Config:         opt.Config,
		KubeClient:     opt.KubeClient,
		StashClient:    opt.StashClient.(*cs.Clientset),
		Namespace:      opt.Namespace,
		RestoreSession: restoreSession.Name,
		Repository:     restoreSession.Spec.Repository.Name,
		Metrics:        opt.Metrics,
		Error:          restoreErr,
	}

	err = o.UpdatePostRestoreStatus(restoreOutput)
	if err != nil {
		return err
	}
	glog.Info("Restore has been completed successfully")
	return nil
}

func HandleRestoreFailure(opt *Options, restoreErr error) error {
	restoreSession, err := opt.StashClient.StashV1beta1().RestoreSessions(opt.Namespace).Get(opt.RestoreSessionName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	host, err := util.GetHostName(restoreSession.Spec.Target)
	if err != nil {
		return err
	}

	var errorMsg string
	if restoreErr != nil {
		errorMsg = restoreErr.Error()
	}
	restoreOutput := &restic.RestoreOutput{
		HostRestoreStats: []api_v1beta1.HostRestoreStats{
			{
				Hostname: host,
				Phase:    api_v1beta1.HostRestoreFailed,
				Error:    errorMsg,
			},
		},
	}
	statusOpt := status.UpdateStatusOptions{
		Config:         opt.Config,
		KubeClient:     opt.KubeClient,
		StashClient:    opt.StashClient,
		Namespace:      opt.Namespace,
		RestoreSession: opt.RestoreSessionName,
		Metrics:        opt.Metrics,
		Error:          restoreErr,
	}

	return statusOpt.UpdatePostRestoreStatus(restoreOutput)
}

func (opt *Options) writeRestoreFailureEvent(restoreSession *api_v1beta1.RestoreSession, host string, err error) {
	// write failure event
	ref, rerr := reference.GetReference(stash_scheme.Scheme, restoreSession)
	if rerr == nil {
		eventer.CreateEventWithLog(
			opt.KubeClient,
			eventer.EventSourceRestoreInitContainer,
			ref,
			core.EventTypeWarning,
			eventer.EventReasonHostRestoreFailed,
			fmt.Sprintf("Failed to restore for host %q. Reason: %v", host, err),
		)
	} else {
		log.Errorf("Failed to write failure event. Reason: %v", rerr)
	}
}

func (opt *Options) isRestoredForThisHost(restoreSession *api_v1beta1.RestoreSession, host string) bool {

	// if overall restoreSession phase is "Succeeded" or "Failed" then it has been processed already
	if restoreSession.Status.Phase == api_v1beta1.RestoreSessionSucceeded ||
		restoreSession.Status.Phase == api_v1beta1.RestoreSessionFailed {
		return true
	}

	// if restoreSession has entry for this host in status field, then it has been already processed for this host
	for _, hostStats := range restoreSession.Status.Stats {
		if hostStats.Hostname == host {
			return true
		}
	}

	return false
}
