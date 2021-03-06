package main

import (
	"flag"
	"fmt"

	"github.com/enix/dothill-csi/pkg/common"
	"github.com/enix/dothill-csi/pkg/controller"
	"k8s.io/klog"
)

var bind = flag.String("bind", fmt.Sprintf("unix:///var/lib/kubelet/plugins/%s/csi.sock", common.PluginName), "RPC bind URI (can be a UNIX socket path or any URI)")

func main() {
	klog.InitFlags(nil)
	flag.Set("logtostderr", "true")
	flag.Parse()
	klog.Infof("starting dothill storage controller %s", common.Version)
	common.NewDriver(controller.NewDriver()).Start(*bind)
}
