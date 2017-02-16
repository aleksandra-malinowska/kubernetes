package upgrades

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/test/e2e/common"
	"k8s.io/kubernetes/test/e2e/framework"
)

type EventsUpgradeTest struct {
	rc      *common.ResourceConsumer
	options metav1.ListOptions
	events  *v1.EventList
}

func (t *EventsUpgradeTest) Setup(f *framework.Framework) {
	t.rc = common.NewDynamicResourceConsumer(
		"events-upgrade-test",
		common.KindRC,
		1,
		20,
		0,
		0,
		200,
		10,
		f,
	)

	t.options = metav1.ListOptions{}
	events, err := f.ClientSet.Core().Events(f.Namespace.Name).List(t.options)
	t.events = events
	if err != nil {
		fmt.Errorf("Error getting events before upgrade, %v", err)
	}
}

func (t *EventsUpgradeTest) Test(f *framework.Framework, done <-chan struct{}, upgrade UpgradeType) {
	<-done
	events, err := f.ClientSet.Core().Events(f.Namespace.Name).List(t.options)
	if err != nil {
		fmt.Errorf("Error getting events after upgrade, %v", err)
	}

	if len(events.Items) < len(t.events.Items) {
		fmt.Errorf("Fewer events after upgrade than before, was: %v, is: %v", len(t.events.Items), len(events.Items))
	}

	for i, _ := range t.events.Items {
		t.compare(t.events.Items[i], events.Items[i])
	}
}

func (t *EventsUpgradeTest) Teardown(f *framework.Framework) {
}

func (t *EventsUpgradeTest) compare(e1 v1.Event, e2 v1.Event) {

}
