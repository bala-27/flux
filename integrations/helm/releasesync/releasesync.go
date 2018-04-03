/*
When a Chart release is deleted/upgraded/created manually, the cluster gets out of sync
with the prescribed state defined in the get repo.
*/
package releasesync

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	protobuf "github.com/golang/protobuf/ptypes/timestamp"
	"github.com/weaveworks/flux/integrations/helm/chartsync"
	"github.com/weaveworks/flux/integrations/helm/release"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	hapi_release "k8s.io/helm/pkg/proto/hapi/release"

	"github.com/go-kit/kit/log"

	ifv1 "github.com/weaveworks/flux/apis/helm.integrations.flux.weave.works/v1alpha"
	ifclientset "github.com/weaveworks/flux/integrations/client/clientset/versioned" // kubernetes 1.9
	helmgit "github.com/weaveworks/flux/integrations/helm/git"
	chartrelease "github.com/weaveworks/flux/integrations/helm/release"
)

type action string

const (
	CustomResourceKind        = "FluxHelmRelease"
	releaseLagTime            = 30
	deleteAction       action = "DELETE"
	installAction      action = "CREATE"
	upgradeAction      action = "UPDATE"
)

type ReleaseFhr struct {
	RelName string
	FhrName string
	Fhr     ifv1.FluxHelmRelease
}

// ReleaseChangeSync will become a receiver, that contains
//
type ReleaseChangeSync struct {
	logger log.Logger
	chartsync.Polling
	kubeClient kubernetes.Clientset
	ifClient   ifclientset.Clientset
	release    *chartrelease.Release
}

func New(
	logger log.Logger, syncInterval time.Duration, syncTimeout time.Duration,
	kubeClient kubernetes.Clientset, ifClient ifclientset.Clientset,
	release *chartrelease.Release) *ReleaseChangeSync {

	return &ReleaseChangeSync{
		logger:     logger,
		Polling:    chartsync.Polling{Interval: syncInterval, Timeout: syncTimeout},
		kubeClient: kubeClient,
		ifClient:   ifClient,
		release:    release,
	}
}

type customResourceInfo struct {
	name, releaseName string
	resource          ifv1.FluxHelmRelease
	lastUpdated       protobuf.Timestamp
}

type chartRelease struct {
	releaseName  string
	action       action
	desiredState ifv1.FluxHelmRelease
}

// Run ... creates a syncing loop monitoring repo chart changes
func (rs *ReleaseChangeSync) Run(stopCh <-chan struct{}, errc chan error, wg *sync.WaitGroup) {
	rs.logger.Log("info", "Starting repo charts sync loop")

	wg.Add(1)
	go func() {
		defer runtime.HandleCrash()
		defer wg.Done()
		defer rs.release.Repo.ReleasesSync.Cleanup()

		time.Sleep(30 * time.Second)

		ticker := time.NewTicker(rs.Polling.Interval)
		defer ticker.Stop()

		for {
			select {
			// ------------------------------------------------------------------------------------
			case <-ticker.C:
				rs.logger.Log("info", fmt.Sprintf("Start of releasesync at %s", time.Now().String()))
				ctx, cancel := context.WithTimeout(context.Background(), helmgit.DefaultCloneTimeout)
				relsToSync, err := rs.releasesToSync(ctx)
				cancel()
				if err != nil {
					rs.logger.Log("error", fmt.Sprintf("Failure to get info about manual chart release changes: %#v", err))
					rs.logger.Log("info", fmt.Sprintf("End of releasesync at %s", time.Now().String()))
					continue
				}

				// manual chart release changes?
				if len(relsToSync) == 0 {
					rs.logger.Log("info", fmt.Sprint("No manual changes of Chart releases"))
					rs.logger.Log("info", fmt.Sprintf("End of releasesync at %s", time.Now().String()))
					continue
				}

				// sync Chart releases
				ctx, cancel = context.WithTimeout(context.Background(), helmgit.DefaultCloneTimeout)
				err = rs.sync(ctx, relsToSync)
				cancel()
				if err != nil {
					rs.logger.Log("error", fmt.Sprintf("Failure to sync cluster after manual chart release changes: %#v", err))
				}
				rs.logger.Log("info", fmt.Sprintf("End of releasesync at %s", time.Now().String()))
			// ------------------------------------------------------------------------------------
			case <-stopCh:
				rs.logger.Log("stopping", "true")
				break
			}
		}
	}()
}

func (rs *ReleaseChangeSync) getNSCustomResources(ns string) (*ifv1.FluxHelmReleaseList, error) {
	return rs.ifClient.HelmV1alpha().FluxHelmReleases(ns).List(metav1.ListOptions{})
}

func (rs *ReleaseChangeSync) getNSEvents(ns string) (*v1.EventList, error) {
	return rs.kubeClient.CoreV1().Events(ns).List(metav1.ListOptions{})
}

// getCustomResources retrieves FluxHelmRelease resources
//		and outputs them organised by namespace and: Chart release name or Custom Resource name
//						map[namespace] = []ReleaseFhr
func (rs *ReleaseChangeSync) getCustomResources(namespaces []string) (map[string][]ReleaseFhr, error) {
	relInfo := make(map[string][]ReleaseFhr)

	for _, ns := range namespaces {
		list, err := rs.getNSCustomResources(ns)
		if err != nil {
			rs.logger.Log("error", fmt.Errorf("Failure while retrieving FluxHelmReleases in namespace %s: %v", ns, err))
			return nil, err
		}
		rf := []ReleaseFhr{}
		for _, fhr := range list.Items {
			relName := release.GetReleaseName(fhr)
			rf = append(rf, ReleaseFhr{RelName: relName, Fhr: fhr})
		}
		if len(rf) > 0 {
			relInfo[ns] = rf
		}
	}
	return relInfo, nil
}

// GetCustomResourcesLastDeploy retrieves and stores last event timestamp for FluxHelmRelease resources (FHR)
// (Tamara: The method is not currently used. I am leaving it here as it will come handy for display on the UI)
//		output:
//             map[namespace][FHR name] = int64
func (rs *ReleaseChangeSync) GetCustomResourcesLastDeploy(namespaces []string) (map[string]map[string]int64, error) {
	relEventsTime := make(map[string]map[string]int64)

	for _, ns := range namespaces {
		eventList, err := rs.getNSEvents(ns)
		if err != nil {
			return relEventsTime, err
		}
		fhrD := make(map[string]int64)
		for _, e := range eventList.Items {
			if e.InvolvedObject.Kind == CustomResourceKind {
				secs := e.LastTimestamp.Unix()
				fhrD[e.InvolvedObject.Name] = secs
			}
		}
		relEventsTime[ns] = fhrD
	}
	return relEventsTime, nil
}

func (rs *ReleaseChangeSync) shouldUpgrade(currRel *hapi_release.Release, fhr ifv1.FluxHelmRelease) (bool, error) {
	currVals := currRel.GetConfig().GetRaw()
	currChart := currRel.GetChart().String()

	// Get the desired release state
	opts := chartrelease.InstallOptions{DryRun: true}
	tempRelName := strings.Join([]string{currRel.GetName(), "temp"}, "-")
	desRel, err := rs.release.Install(rs.release.Repo.ReleasesSync, tempRelName, fhr, "CREATE", opts)
	if err != nil {
		return false, err
	}
	desVals := desRel.GetConfig().GetRaw()
	desChart := desRel.GetChart().String()

	// compare values && Chart
	if strings.Compare(currVals, desVals) != 0 {
		rs.logger.Log("error", fmt.Sprintf("Release %s: values have diverged due to manual Chart release", currRel.GetName()))
		return true, nil
	}
	if strings.Compare(currChart, desChart) != 0 {
		rs.logger.Log("error", fmt.Sprintf("Release %s: Chart has diverged due to manual Chart release", currRel.GetName()))
		return true, nil
	}

	return false, nil
}

// existingReleasesToSync determines which Chart releases need to be deleted/upgraded
// to bring the cluster to the desired state
func (rs *ReleaseChangeSync) existingReleasesToSync(
	currentReleases map[string]map[string]int64,
	customResources map[string]map[string]ifv1.FluxHelmRelease,
	relsToSync map[string][]chartRelease) error {

	var chRels []chartRelease
	for ns, nsRelsM := range currentReleases {
		chRels = relsToSync[ns]
		for relName := range nsRelsM {
			fhr, ok := customResources[ns][relName]
			if !ok {
				chr := chartRelease{
					releaseName:  relName,
					action:       deleteAction,
					desiredState: fhr,
				}
				chRels = append(chRels, chr)

			} else {
				rel, err := rs.release.GetDeployedRelease(relName)
				if err != nil {
					return err
				}
				doUpgrade, err := rs.shouldUpgrade(rel, fhr)
				if err != nil {
					return err
				}
				if doUpgrade {
					chr := chartRelease{
						releaseName:  relName,
						action:       upgradeAction,
						desiredState: fhr,
					}
					chRels = append(chRels, chr)
				}
			}
		}
		if len(chRels) > 0 {
			relsToSync[ns] = chRels
		}
	}
	return nil
}

// deletedReleasesToSync determines which Chart releases need to be installed
// to bring the cluster to the desired state
func (rs *ReleaseChangeSync) deletedReleasesToSync(
	customResources map[string]map[string]ifv1.FluxHelmRelease,
	currentReleases map[string]map[string]int64,
	relsToSync map[string][]chartRelease) error {

	var chRels []chartRelease
	for ns, nsFhrs := range customResources {
		chRels = relsToSync[ns]

		for relName, fhr := range nsFhrs {
			// there are Custom Resources (CRs) in this namespace
			// missing Chart release even though there is a CR
			if _, ok := currentReleases[ns][relName]; !ok {
				chr := chartRelease{
					releaseName:  relName,
					action:       installAction,
					desiredState: fhr,
				}
				chRels = append(chRels, chr)
			}
		}
		if len(chRels) > 0 {
			relsToSync[ns] = chRels
		}
	}
	return nil
}

// releasesToSync gathers all releases that need syncing
func (rs *ReleaseChangeSync) releasesToSync(ctx context.Context) (map[string][]chartRelease, error) {
	ns, err := chartsync.GetNamespaces(rs.logger, rs.kubeClient)
	if err != nil {
		return nil, err
	}
	relDepl, err := rs.release.GetCurrentWithDate()
	if err != nil {
		return nil, err
	}
	curRels := MappifyDeployInfo(relDepl)

	relCrs, err := rs.getCustomResources(ns)
	if err != nil {
		return nil, err
	}
	crs := MappifyReleaseFhrInfo(relCrs)

	relsToSync := make(map[string][]chartRelease)
	rs.deletedReleasesToSync(crs, curRels, relsToSync)
	rs.existingReleasesToSync(curRels, crs, relsToSync)

	return relsToSync, nil
}

// sync deletes/upgrades/installs a Chart release
func (rs *ReleaseChangeSync) sync(ctx context.Context, releases map[string][]chartRelease) error {

	checkout := rs.release.Repo.ReleasesSync
	opts := chartrelease.InstallOptions{DryRun: false}
	for ns, relsToProcess := range releases {
		for _, chr := range relsToProcess {
			relName := chr.releaseName
			switch chr.action {
			case deleteAction:
				rs.logger.Log("info", fmt.Sprintf("Deleting manually installed Chart release %s (namespace %s)", relName, ns))
				err := rs.release.Delete(relName)
				if err != nil {
					return err
				}
			case upgradeAction:
				rs.logger.Log("info", fmt.Sprintf("Resyncing manually upgraded Chart release %s (namespace %s)", relName, ns))
				_, err := rs.release.Install(checkout, relName, chr.desiredState, chartrelease.ReleaseType("UPDATE"), opts)
				if err != nil {
					return err
				}
			case installAction:
				rs.logger.Log("info", fmt.Sprintf("Installing manually deleted Chart release %s (namespace %s)", relName, ns))
				_, err := rs.release.Install(checkout, relName, chr.desiredState, chartrelease.ReleaseType("CREATE"), opts)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
