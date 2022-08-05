package logset

import (
	"reflect"

	"github.com/matrixorigin/matrixone-operator/api/core/v1alpha1"
	"github.com/matrixorigin/matrixone-operator/pkg/controllers/common"
	recon "github.com/matrixorigin/matrixone-operator/runtime/pkg/reconciler"
	"github.com/matrixorigin/matrixone-operator/runtime/pkg/util"
	kruisev1 "github.com/openkruise/kruise-api/apps/v1beta1"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	BootstrapAnnoKey = "logset.matrixorigin.io/bootstrap"

	IDRangeStart int = 131072
	IDRangeEnd   int = 262144

	ReasonNoEnoughReadyStores = "NoEnoughReadyStores"
)

var _ recon.Actor[*v1alpha1.LogSet] = &LogSetActor{}

type LogSetActor struct{}

type WithResources struct {
	*LogSetActor
	sts *kruisev1.StatefulSet
}

func (r *LogSetActor) with(sts *kruisev1.StatefulSet) *WithResources {
	return &WithResources{LogSetActor: r, sts: sts}
}

func (r *LogSetActor) Observe(ctx *recon.Context[*v1alpha1.LogSet]) (recon.Action[*v1alpha1.LogSet], error) {
	ls := ctx.Obj

	// get subresources
	discoverySvc := &corev1.Service{}
	err, foundDiscovery := util.IsFound(ctx.Get(client.ObjectKey{Namespace: ls.Namespace, Name: discoverySvcName(ls)}, discoverySvc))
	if err != nil {
		return nil, errors.Wrap(err, "get HAKeeper discovery service")
	}
	sts := &kruisev1.StatefulSet{}
	err, foundSts := util.IsFound(ctx.Get(client.ObjectKey{Namespace: ls.Namespace, Name: stsName(ls)}, sts))
	if err != nil {
		return nil, errors.Wrap(err, "get logservice statefulset")
	}
	if !foundDiscovery || !foundSts {
		return r.Create, nil
	}

	// calculate status
	podList := &corev1.PodList{}
	err = ctx.List(podList, client.InNamespace(ls.Namespace),
		client.MatchingLabels(common.SubResourceLabels(ls)))
	if err != nil {
		return nil, errors.Wrap(err, "list logservice pods")
	}

	collectStoreStatus(ls, podList.Items)
	if len(ls.Status.AvailableStores) >= int(ls.Spec.Replicas) {
		ls.Status.SetCondition(metav1.Condition{
			Type:   v1alpha1.ConditionTypeReady,
			Status: metav1.ConditionTrue,
		})
	} else {
		ls.Status.SetCondition(metav1.Condition{
			Type:   v1alpha1.ConditionTypeReady,
			Status: metav1.ConditionFalse,
			Reason: ReasonNoEnoughReadyStores,
		})
	}
	ls.Status.Discovery = &v1alpha1.LogSetDiscovery{
		Port:    LogServicePort,
		Address: discoverySvcAddress(ls),
	}

	switch {
	case len(ls.Status.FailedStores) > 0:
		return r.Repair, nil
	case ls.Spec.Replicas != *sts.Spec.Replicas:
		return r.with(sts).Scale, nil
	}
	origin := sts.DeepCopy()
	if err := syncPods(ctx, sts); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(origin, sts) {
		return r.with(sts).Update, nil
	}
	return nil, nil
}

func (r *LogSetActor) Create(ctx *recon.Context[*v1alpha1.LogSet]) error {
	ls := ctx.Obj

	// build resources required by a logset
	bc, err := buildBootstrapConfig(ctx)
	if err != nil {
		return err
	}
	svc := buildHeadlessSvc(ls)
	sts := buildStatefulSet(ls, svc)
	syncReplicas(ls, sts)
	syncPodMeta(ls, sts)
	syncPodSpec(ls, sts)
	syncPersistentVolumeClaim(ls, sts)
	discovery := buildDiscoveryService(ls)

	// sync the config
	cm, err := buildConfigMap(ls)
	if err != nil {
		return err
	}
	if err := common.SyncConfigMap(ctx, &sts.Spec.Template.Spec, cm); err != nil {
		return err
	}

	// create all resources
	err = lo.Reduce[client.Object, error]([]client.Object{
		bc,
		svc,
		sts,
		discovery,
	}, func(errs error, o client.Object, _ int) error {
		err := ctx.CreateOwned(o)
		// ignore already exist during creation, updating of the underlying resources should be
		// done carefully in other Actions since updating might be destructive
		return multierr.Append(errs, util.Ignore(apierrors.IsAlreadyExists, err))
	}, nil)
	if err != nil {
		return errors.Wrap(err, "create")
	}
	return nil
}

// Scale scale-out/in the log set pods to match the desired state
// TODO(aylei): special treatment for scale-in
func (r *WithResources) Scale(ctx *recon.Context[*v1alpha1.LogSet]) error {
	return ctx.Patch(r.sts, func() error {
		syncReplicas(ctx.Obj, r.sts)
		return nil
	})
}

// Repair repairs failed log set pods to match the desired state
func (r *LogSetActor) Repair(ctx *recon.Context[*v1alpha1.LogSet]) error {
	// TODO(aylei): implement
	return nil
}

// Update rolling-update the log set pods to match the desired state
// TODO(aylei): should logset controller take care of graceful rolling?
func (r *WithResources) Update(ctx *recon.Context[*v1alpha1.LogSet]) error {
	return ctx.Update(r.sts)
}

func (r *LogSetActor) Finalize(ctx *recon.Context[*v1alpha1.LogSet]) (bool, error) {
	ls := ctx.Obj
	var errs error
	// subresources should be deleted by owner reference, simply wait the deletion complete
	svcExist, err := ctx.Exist(client.ObjectKey{Namespace: ls.Namespace, Name: headlessSvcName(ls)}, &corev1.Service{})
	errs = multierr.Append(errs, err)
	stsExist, err := ctx.Exist(client.ObjectKey{Namespace: ls.Namespace, Name: stsName(ls)}, &kruisev1.StatefulSet{})
	errs = multierr.Append(errs, err)
	discoverySvcExist, err := ctx.Exist(client.ObjectKey{Namespace: ls.Namespace, Name: stsName(ls)}, &corev1.Service{})
	errs = multierr.Append(errs, err)
	return (!svcExist) && (!stsExist) && (!discoverySvcExist), errs
}

func syncPods(ctx *recon.Context[*v1alpha1.LogSet], sts *kruisev1.StatefulSet) error {
	cm, err := buildConfigMap(ctx.Obj)
	if err != nil {
		return err
	}
	syncPodMeta(ctx.Obj, sts)
	syncPodSpec(ctx.Obj, sts)
	return common.SyncConfigMap(ctx, &sts.Spec.Template.Spec, cm)
}