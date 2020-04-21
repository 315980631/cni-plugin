// Copyright 2015 Tigera Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package ipamplugin

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/projectcalico/cni-plugin/pkg/k8s"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"net"
	"os"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	cniSpecVersion "github.com/containernetworking/cni/pkg/version"
	"github.com/projectcalico/cni-plugin/internal/pkg/utils"
	"github.com/projectcalico/cni-plugin/pkg/types"
	"github.com/projectcalico/cni-plugin/pkg/upgrade"
	"github.com/projectcalico/libcalico-go/lib/apiconfig"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/ipam"
	"github.com/projectcalico/libcalico-go/lib/logutils"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Main(version string) {
	// Set up logging formatting.
	logrus.SetFormatter(&logutils.Formatter{})

	// Install a hook that adds file/line no information.
	logrus.AddHook(&logutils.ContextHook{})

	// Display the version on "-v", otherwise just delegate to the skel code.
	// Use a new flag set so as not to conflict with existing libraries which use "flag"
	flagSet := flag.NewFlagSet("calico-ipam", flag.ExitOnError)

	versionFlag := flagSet.Bool("v", false, "Display version")
	upgradeFlag := flagSet.Bool("upgrade", false, "Upgrade from host-local")
	err := flagSet.Parse(os.Args[1:])

	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if *versionFlag {
		fmt.Println(version)
		os.Exit(0)
	}

	// Migration logic
	if *upgradeFlag {
		logrus.Info("migrating from host-local to calico-ipam...")
		ctxt := context.Background()

		// nodename associates IPs to this node.
		nodename := os.Getenv("KUBERNETES_NODE_NAME")
		if nodename == "" {
			logrus.Fatal("KUBERNETES_NODE_NAME not specified, refusing to migrate...")
		}
		logCtxt := logrus.WithField("node", nodename)

		// calicoClient makes IPAM calls.
		cfg, err := apiconfig.LoadClientConfig("")
		if err != nil {
			logCtxt.Fatal("failed to load api client config")
		}
		cfg.Spec.DatastoreType = apiconfig.Kubernetes
		calicoClient, err := client.New(*cfg)
		if err != nil {
			logCtxt.Fatal("failed to initialize api client")
		}

		// Perform the migration.
		for {
			err := upgrade.Migrate(ctxt, calicoClient, nodename)
			if err == nil {
				break
			}
			logCtxt.WithError(err).Error("failed to migrate ipam, retrying...")
			time.Sleep(time.Second)
		}
		logCtxt.Info("migration from host-local to calico-ipam complete")
		os.Exit(0)
	}

	skel.PluginMain(cmdAdd, nil, cmdDel,
		cniSpecVersion.PluginSupports("0.1.0", "0.2.0", "0.3.0", "0.3.1"),
		"Calico CNI IPAM "+version)
}

type ipamArgs struct {
	cnitypes.CommonArgs
	IP net.IP `json:"ip,omitempty"`
}

//申请IP
func cmdAdd(args *skel.CmdArgs) error {
	conf := types.NetConf{}
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("failed to load netconf: %v", err)
	}

	nodename := utils.DetermineNodename(conf)

	utils.ConfigureLogging(conf.LogLevel)

	calicoClient, err := utils.CreateClient(conf)
	if err != nil {
		return err
	}

	epIDs, err := utils.GetIdentifiers(args, nodename)
	if err != nil {
		return err
	}

	epIDs.WEPName, err = epIDs.CalculateWorkloadEndpointName(false)
	if err != nil {
		return fmt.Errorf("error constructing WorkloadEndpoint name: %s", err)
	}

	//这里的containerId为pause容器ID
	handleID := utils.GetHandleID(conf.Name, args.ContainerID, epIDs.WEPName)

	logger := logrus.WithFields(logrus.Fields{
		"Workload":    epIDs.WEPName,
		"ContainerID": epIDs.ContainerID,
		"HandleID":    handleID,
	})

	ipamArgs := ipamArgs{}
	if err = cnitypes.LoadArgs(args.Args, &ipamArgs); err != nil {
		return err
	}

	// We attach important attributes to the allocation.
	// 我们将重要属性附加到分配中包括pod的namespace和name
	attrs := map[string]string{ipam.AttributeNode: nodename}
	if epIDs.Pod != "" {
		attrs[ipam.AttributePod] = epIDs.Pod
		attrs[ipam.AttributeNamespace] = epIDs.Namespace
	}

	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	r := &current.Result{}
	// 如果是指定 IP分配
	if ipamArgs.IP != nil {
		logger.Infof("Calico CNI IPAM request IP: %v", ipamArgs.IP)

		assignArgs := ipam.AssignIPArgs{
			IP:       cnet.IP{IP: ipamArgs.IP},
			HandleID: &handleID,
			Hostname: nodename,
			Attrs:    attrs,
		}
		logger.WithField("assignArgs", assignArgs).Info("Assigning provided IP")
		err := calicoClient.IPAM().AssignIP(ctx, assignArgs)
		if err != nil {
			return err
		}

		var ipNetwork net.IPNet

		if ipamArgs.IP.To4() == nil {
			// It's an IPv6 address.
			ipNetwork = net.IPNet{IP: ipamArgs.IP, Mask: net.CIDRMask(128, 128)}
			r.IPs = append(r.IPs, &current.IPConfig{
				Version: "6",
				Address: ipNetwork,
			})

			logger.WithField("result.IPs", ipamArgs.IP).Info("Appending an IPv6 address to the result")
		} else {
			// It's an IPv4 address.
			ipNetwork = net.IPNet{IP: ipamArgs.IP, Mask: net.CIDRMask(32, 32)}
			r.IPs = append(r.IPs, &current.IPConfig{
				Version: "4",
				Address: ipNetwork,
			})

			logger.WithField("result.IPs", ipamArgs.IP).Info("Appending an IPv4 address to the result")
		}
	} else {
		// 创建client set
		clientset, err := k8s.NewK8sClient(conf, logger)
		if err != nil {
			logger.Errorf("New K8s Client error: %v", err.Error())
			return err
		}
		isFixedIP, stsName, err := utils.VariatePodFixedIP(clientset, epIDs.Pod, epIDs.Namespace)
		if err != nil {
			logger.Errorf("VariatePodFixedIP error: %v", err.Error())
			return err
		}

		// Default to assigning an IPv4 and IPv6 ""
		ipv4 := ""
		ipv6 := ""

		// Default to assigning an IPv4 address
		num4 := 1
		if conf.IPAM.AssignIpv4 != nil && *conf.IPAM.AssignIpv4 == "false" {
			num4 = 0
		}

		// Default to NOT assigning an IPv6 address
		num6 := 0
		if conf.IPAM.AssignIpv6 != nil && *conf.IPAM.AssignIpv6 == "true" {
			num6 = 1
		}

		logger.Infof("Calico CNI IPAM request count IPv4=%d IPv6=%d", num4, num6)

		//calico二进制会将pod或者namespaces中的ipv4pool注解放到conf.IPAM.IPv4Pools中，如["default-ipv4-ippool", "test1-ipv4-ippool"]
		v4pools, err := utils.ResolvePools(ctx, calicoClient, conf.IPAM.IPv4Pools, true)
		if err != nil {
			return err
		}
		//calico二进制会将pod或者namespaces中的ipv6pool注解放到conf.IPAM.IPv6Pools中
		v6pools, err := utils.ResolvePools(ctx, calicoClient, conf.IPAM.IPv6Pools, false)
		if err != nil {
			return err
		}

		logger.Infof("Calico CNI IPAM handle=%s", handleID)
		var excludeIPs []cnet.IPNet

		assignArgs := ipam.AutoAssignArgs{
			Num4:      num4,
			Num6:      num6,
			HandleID:  &handleID,
			Hostname:  nodename,
			IPv4Pools: v4pools,
			IPv6Pools: v6pools,
			Attrs:     attrs,
			//排除掉固定IP的
			ExcludeIPs: excludeIPs,
		}
		logger.WithField("assignArgs", assignArgs).Info("Auto assigning IP")

		flagLabel := map[string]string{"calico-fixedIP": "true"}
		allFixedIPCms, err := clientset.CoreV1().ConfigMaps("").List(metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(flagLabel).String(),
		})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				//其他错误
				return fmt.Errorf("get fixed ip configmaps error: %v", err)
			}
			//不存在
		}
		configName := stsName + "-fixed-ip-conf"
		var thisFixedCm v1.ConfigMap
		for _, item := range allFixedIPCms.Items {
			for podIPVesion, fixedIP := range item.Data {
				//被固定IP占用的ipv4和ipv6地址
				ip := net.ParseIP(fixedIP)
				if strings.HasSuffix(podIPVesion, k8s.IPV4Suffix) && ip != nil && ip.To4() != nil {
					//排除ipv4
					excludeIPs = append(excludeIPs, cnet.IPNet{IPNet: net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}})
				} else if strings.HasSuffix(podIPVesion, k8s.IPV6Suffix) && ip != nil && ip.To4() == nil {
					//排除ipv6
					excludeIPs = append(excludeIPs, cnet.IPNet{IPNet: net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}})
				} else {
					return fmt.Errorf("assgin IP to Pod: %v/%v find configmaps: %v/%v error fixedIP data: %v", epIDs.Namespace, epIDs.Pod,
						item.Namespace, item.Name, fmt.Sprintf("%v: %v", podIPVesion, fixedIP))
				}
			}
			if epIDs.Namespace == item.Namespace && configName == item.Name {
				thisFixedCm = item
			}
		}
		// 判断Pod是否为sts且开启固定IP，如果是则查看是否存在对应的configMap，没有则创建对应的configMap
		if isFixedIP {
			logger.Infof("Pod enable fixed IP and kind is statefulset: %v", isFixedIP)
			refLabel := map[string]string{"calico-fixedIP-ref": stsName}
			if thisFixedCm.Name != "" {
				//存在cm
				logger.Infof("configMap is %v", thisFixedCm)
				var ips []net.IPNet
				ipv4Addr := net.IPNet{IP: net.ParseIP(thisFixedCm.Data[epIDs.Pod+k8s.IPV4Suffix]), Mask: net.CIDRMask(32, 32)}
				ipv6Addr := net.IPNet{IP: net.ParseIP(thisFixedCm.Data[epIDs.Pod+k8s.IPV6Suffix]), Mask: net.CIDRMask(128, 128)}
				ips = append(ips, ipv4Addr, ipv6Addr)

				if ipv4Addr.IP != nil || ipv6Addr.IP != nil {
					//存在固定IP
					logger.Infof("IPv4: %v, IPv6: %v", ipv4Addr.IP, ipv6Addr.IP)
					for _, ip := range ips {
						if ip.IP != nil {
							version := "4"
							if ip.IP.To4() == nil {
								version = "6"
							}
							//申请旧IP
							err := calicoClient.IPAM().AssignIP(ctx, ipam.AssignIPArgs{
								IP: cnet.IP{ip.IP}, HandleID: assignArgs.HandleID, Attrs: assignArgs.Attrs, Hostname: nodename})
							if err != nil {
								//申请旧IP error
								return fmt.Errorf("calico apply old sts ip error: %v", err)
							}

							//申请成功
							r.IPs = append(r.IPs, &current.IPConfig{
								Version: version,
								Address: ip,
							})
						}
					}

					// Print result to stdout, in the format defined by the requested cniVersion.
					return cnitypes.PrintResult(r, conf.CNIVersion)
				}
			} else {
				//不存在cm
				logger.Infof("start create configMap %v", configName)
				configMap := &v1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:        configName,
						Namespace:   epIDs.Namespace,
						Annotations: refLabel,
						Labels:      labels.Merge(refLabel, flagLabel),
					},
					Data: make(map[string]string),
				}

				retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
					_, err := clientset.CoreV1().ConfigMaps(epIDs.Namespace).Create(configMap)
					return err
				})
				if retryErr != nil {
					return fmt.Errorf("create fixed ip configmaps error: %v", err)
				}
				logger.Infof("create %v configMap success", configName)
			}
		}

		//从排除掉固定IP的范围中分配
		assignedV4, assignedV6, err := calicoClient.IPAM().AutoAssign(ctx, assignArgs)
		logger.Infof("Calico CNI IPAM assigned addresses IPv4=%v IPv6=%v", assignedV4, assignedV6)
		if err != nil {
			return err
		}

		if num4 == 1 {
			if len(assignedV4) != num4 {
				return fmt.Errorf("failed to request %d IPv4 addresses. IPAM allocated only %d", num4, len(assignedV4))
			}
			ipV4Network := net.IPNet{IP: assignedV4[0].IP, Mask: net.CIDRMask(32, 32)}
			r.IPs = append(r.IPs, &current.IPConfig{
				Version: "4",
				Address: ipV4Network,
			})
			ipv4 = assignedV4[0].IP.String()
		}

		if num6 == 1 {
			if len(assignedV6) != num6 {
				return fmt.Errorf("failed to request %d IPv6 addresses. IPAM allocated only %d", num6, len(assignedV6))
			}
			ipV6Network := net.IPNet{IP: assignedV6[0].IP, Mask: net.CIDRMask(128, 128)}
			r.IPs = append(r.IPs, &current.IPConfig{
				Version: "6",
				Address: ipV6Network,
			})
			ipv6 = assignedV6[0].IP.String()
		}

		//分配完IP后更新cm
		if isFixedIP {
			retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				configMap, err := clientset.CoreV1().ConfigMaps(epIDs.Namespace).Get(configName, metav1.GetOptions{})
				if err != nil {
					panic(err)
				}
				logger.Infof("old configMap is %v", configMap)
				if configMap.Data == nil {
					data := make(map[string]string)
					data[epIDs.Pod+k8s.IPV4Suffix] = ipv4
					data[epIDs.Pod+k8s.IPV6Suffix] = ipv6
					configMap.Data = data
				} else {
					configMap.Data[epIDs.Pod+k8s.IPV4Suffix] = ipv4
					configMap.Data[epIDs.Pod+k8s.IPV6Suffix] = ipv6
				}
				logger.Infof("start to update configMap to %v", configMap)
				_, err = clientset.CoreV1().ConfigMaps(epIDs.Namespace).Update(configMap)
				return err
			})
			if retryErr != nil {
				return fmt.Errorf("update %v fixed IP configMap failed: %v", configName, retryErr)
			}
		}
		logger.WithFields(logrus.Fields{"result.IPs": r.IPs}).Info("IPAM Result")
	}

	// Print result to stdout, in the format defined by the requested cniVersion.
	return cnitypes.PrintResult(r, conf.CNIVersion)
}

//释放IP
func cmdDel(args *skel.CmdArgs) error {
	conf := types.NetConf{}
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return fmt.Errorf("failed to load netconf: %v", err)
	}

	utils.ConfigureLogging(conf.LogLevel)

	calicoClient, err := utils.CreateClient(conf)
	if err != nil {
		return err
	}

	nodename := utils.DetermineNodename(conf)

	// Release the IP address by using the handle - which is workloadID.
	epIDs, err := utils.GetIdentifiers(args, nodename)
	if err != nil {
		return err
	}

	epIDs.WEPName, err = epIDs.CalculateWorkloadEndpointName(false)
	if err != nil {
		return fmt.Errorf("error constructing WorkloadEndpoint name: %s", err)
	}

	handleID := utils.GetHandleID(conf.Name, args.ContainerID, epIDs.WEPName)

	logger := logrus.WithFields(logrus.Fields{
		"Workload":    epIDs.WEPName,
		"ContainerID": epIDs.ContainerID,
		"HandleID":    handleID,
	})

	logger.Info("Releasing address using handleID")
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	//释放handleID分配的所有IP地址
	if err := calicoClient.IPAM().ReleaseByHandle(ctx, handleID); err != nil {
		if _, ok := err.(errors.ErrorResourceDoesNotExist); !ok {
			logger.WithError(err).Error("Failed to release address")
			return err
		}
		logger.Warn("Asked to release address but it doesn't exist. Ignoring")
	} else {
		logger.Info("Released address using handleID")
	}

	// Calculate the workloadID to account for v2.x upgrades.
	workloadID := epIDs.ContainerID
	if epIDs.Orchestrator == "k8s" {
		workloadID = fmt.Sprintf("%s.%s", epIDs.Namespace, epIDs.Pod)
	}

	logger.Info("Releasing address using workloadID")
	//释放workloadID分配的所有IP地址
	if err := calicoClient.IPAM().ReleaseByHandle(ctx, workloadID); err != nil {
		if _, ok := err.(errors.ErrorResourceDoesNotExist); !ok {
			logger.WithError(err).Error("Failed to release address")
			return err
		}
		logger.WithField("workloadID", workloadID).Debug("Asked to release address but it doesn't exist. Ignoring")
	} else {
		logger.WithField("workloadID", workloadID).Info("Released address using workloadID")
	}

	return nil
}
