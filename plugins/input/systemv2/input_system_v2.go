// Copyright 2021 iLogtail Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package systemv2

import (
	"github.com/alibaba/ilogtail"
	"github.com/alibaba/ilogtail/helper"
	"github.com/alibaba/ilogtail/pkg/logger"
	"github.com/alibaba/ilogtail/pkg/util"

	"math"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/procfs"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/net"
)

// InputSystem plugin is modified with care, because two collect libs are used， which are procfs and gopsutil.
// They are works well on the host machine. But on the linux virtual environment, they are different.
// The procfs or read proc file system should mount the `logtail_host` path, more details please see `helper.mount_others.go`.
// The gopsutil lib only support mount path with ENV config, more details please see `github.com/shirou/gopsutil/internal/common/common.go`.
type InputSystem struct {
	Core              bool
	CPU               bool
	Mem               bool
	Disk              bool
	Net               bool
	Protocol          bool
	TCP               bool
	OpenFd            bool
	CPUPercent        bool
	Disks             []string
	NetInterfaces     []string
	Labels            map[string]string
	ExcludeDiskFsType string
	ExcludeDiskPath   string

	lastInfo               *host.InfoStat
	lastCPUStat            cpu.TimesStat
	lastCPUTime            time.Time
	lastCPUTotal           float64
	lastCPUBusy            float64
	lastNetStat            net.IOCountersStat
	lastNetStatAll         []net.IOCountersStat
	lastNetTime            time.Time
	lastProtoAll           []net.ProtoCountersStat
	lastProtoTime          time.Time
	lastDiskStat           disk.IOCountersStat
	lastDiskStatAll        map[string]disk.IOCountersStat
	lastDiskTime           time.Time
	commonLabels           helper.KeyValues
	commonLabelsStr        string
	collectTime            time.Time
	context                ilogtail.Context
	excludeDiskFsTypeRegex *regexp.Regexp
	excludeDiskPathRegex   *regexp.Regexp
	//nolint: structcheck,unused
	fs *procfs.FS
}

func (r *InputSystem) Description() string {
	return "Support collect system metrics on the host machine or Linux virtual environments."
}

func (r *InputSystem) CommonInit(context ilogtail.Context) (int, error) {
	if r.ExcludeDiskFsType != "" {
		reg, err := regexp.Compile(r.ExcludeDiskFsType)
		if err != nil {
			logger.Error(r.context.GetRuntimeContext(), "COMPILE_REGEXP_ALARM", "err", err)
			return 0, err
		}
		r.excludeDiskFsTypeRegex = reg
	}
	if r.ExcludeDiskPath != "" {
		reg, err := regexp.Compile(r.ExcludeDiskPath)
		if err != nil {
			logger.Error(r.context.GetRuntimeContext(), "COMPILE_REGEXP_ALARM", "err", err)
			return 0, err
		}
		r.excludeDiskPathRegex = reg
	}
	r.context = context
	r.commonLabels.Append("hostname", util.GetHostName())
	r.commonLabels.Append("ip", util.GetIPAddress())
	for key, val := range r.Labels {
		r.commonLabels.Append(key, val)
	}
	r.commonLabels.Sort()
	r.commonLabelsStr = r.commonLabels.String()
	return 0, nil
}

func (r *InputSystem) addMetric(collector ilogtail.Collector,
	name string,
	labels string,
	value float64) {
	keys, vals := helper.MakeMetric(name, labels, r.collectTime.UnixNano(), value)
	collector.AddDataArray(nil, keys, vals, r.collectTime)
}

func (r *InputSystem) CollectCore(collector ilogtail.Collector) {

	// host info
	if r.lastInfo == nil {
		r.lastInfo, _ = host.Info()
	}

	// load stat
	loadStat, err := load.Avg()
	if err == nil {
		r.addMetric(collector, "system_load1", r.commonLabelsStr, loadStat.Load1)
		r.addMetric(collector, "system_load5", r.commonLabelsStr, loadStat.Load5)
		r.addMetric(collector, "system_load15", r.commonLabelsStr, loadStat.Load15)
	}
	r.addMetric(collector, "system_boot_time", r.commonLabelsStr, float64(r.lastInfo.BootTime))
}

func (r *InputSystem) CollectCPU(collector ilogtail.Collector) {
	// cpu stat
	cpuStat, err := cpu.Times(false)
	cpuInfo, _ := cpu.Info()
	ncpus := int32(0)
	for _, c := range cpuInfo {
		ncpus += c.Cores
	}
	r.addMetric(collector, "cpu_count", r.commonLabelsStr, float64(ncpus))
	if err == nil && len(cpuStat) > 0 {
		cpuBusy := cpuStat[0].GuestNice + cpuStat[0].Guest + cpuStat[0].Nice +
			cpuStat[0].Softirq + cpuStat[0].Irq + cpuStat[0].User + cpuStat[0].System
		cpuTotal := cpuBusy + cpuStat[0].Idle + cpuStat[0].Iowait + cpuStat[0].Steal

		// cpushare计算
		cpuShareFactor := 1.0
		cpushareEnv := os.Getenv("SIGMA_CPU_REQUEST")
		if len(cpushareEnv) > 0 {
			cpuRequest, err := strconv.Atoi(cpushareEnv)
			if err != nil || cpuRequest <= 0 || ncpus == 0 {
				logger.Error(r.context.GetRuntimeContext(), "GET_SIGMA_ENV_ERROR", "get sigma env failed",
					"error", err,
					"ncpus", ncpus,
					"SIGMA_CPU_REQUEST", cpushareEnv)
			} else {
				cpuShareFactor = float64(ncpus) / (float64(cpuRequest) / 1000.)
			}
		}

		deltaTotal := cpuTotal - r.lastCPUTotal
		if r.CPUPercent && !r.lastCPUTime.IsZero() && deltaTotal > 0 {
			r.addMetric(collector, "cpu_util", r.commonLabelsStr, 100*(cpuBusy-r.lastCPUBusy)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_wait_util", r.commonLabelsStr, 100*(cpuStat[0].Iowait-r.lastCPUStat.Iowait)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_sys_util", r.commonLabelsStr, 100*(cpuStat[0].System-r.lastCPUStat.System)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_user_util", r.commonLabelsStr, 100*(cpuStat[0].User-r.lastCPUStat.User)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_irq_util", r.commonLabelsStr, 100*(cpuStat[0].Irq-r.lastCPUStat.Irq)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_softirq_util", r.commonLabelsStr, 100*(cpuStat[0].Softirq-r.lastCPUStat.Softirq)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_nice_util", r.commonLabelsStr, 100*(cpuStat[0].Nice-r.lastCPUStat.Nice)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_steal_util", r.commonLabelsStr, 100*(cpuStat[0].Steal-r.lastCPUStat.Steal)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_guest_util", r.commonLabelsStr, 100*(cpuStat[0].Guest-r.lastCPUStat.Guest)/deltaTotal*cpuShareFactor)
			r.addMetric(collector, "cpu_guestnice_util", r.commonLabelsStr, 100*(cpuStat[0].GuestNice-r.lastCPUStat.GuestNice)/deltaTotal*cpuShareFactor)
		}

		r.lastCPUTime = time.Now()
		r.lastCPUStat = cpuStat[0]
		r.lastCPUBusy = cpuBusy
		r.lastCPUTotal = cpuTotal
	}
}

func (r *InputSystem) CollectMem(collector ilogtail.Collector) {
	// mem stat
	memStat, err := mem.VirtualMemory()
	if err == nil {
		r.addMetric(collector, "mem_util", r.commonLabelsStr, memStat.UsedPercent)
		r.addMetric(collector, "mem_cache", r.commonLabelsStr, float64(memStat.Cached))
		r.addMetric(collector, "mem_free", r.commonLabelsStr, float64(memStat.Free))
		r.addMetric(collector, "mem_available", r.commonLabelsStr, float64(memStat.Available))
		r.addMetric(collector, "mem_used", r.commonLabelsStr, float64(memStat.Used))
		r.addMetric(collector, "mem_total", r.commonLabelsStr, float64(memStat.Total))
	}

	swapStat, err := mem.SwapMemory()
	if err == nil {
		r.addMetric(collector, "mem_swap_util", r.commonLabelsStr, swapStat.UsedPercent)
	}
}

func (r *InputSystem) collectOneDisk(collector ilogtail.Collector, name string, timeDeltaSec float64, last, now *disk.IOCountersStat) {
	newLabels := r.commonLabels.Clone()
	newLabels.Append("disk", name)
	newLabels.Sort()
	labels := newLabels.String()
	r.addMetric(collector, "disk_rbps", labels, float64(now.ReadBytes-last.ReadBytes)/timeDeltaSec)
	r.addMetric(collector, "disk_wbps", labels, float64(now.WriteBytes-last.WriteBytes)/timeDeltaSec)
	r.addMetric(collector, "disk_riops", labels, float64(now.ReadCount-last.ReadCount)/timeDeltaSec)
	r.addMetric(collector, "disk_wiops", labels, float64(now.WriteCount-last.WriteCount)/timeDeltaSec)
	if now.ReadCount-last.ReadCount > 0 {
		r.addMetric(collector, "disk_rlatency", labels, float64(now.ReadTime-last.ReadTime)/float64(now.ReadCount-last.ReadCount))
	} else {
		r.addMetric(collector, "disk_rlatency", labels, math.NaN())
	}
	if now.WriteCount-last.WriteCount > 0 {
		r.addMetric(collector, "disk_wlatency", labels, float64(now.WriteTime-last.WriteTime)/float64(now.WriteCount-last.WriteCount))
	} else {
		r.addMetric(collector, "disk_wlatency", labels, math.NaN())
	}
	if name != "total" {
		r.addMetric(collector, "disk_util", labels, float64(now.IoTime-last.IoTime)*100./1000./timeDeltaSec)
	}
}

func (r *InputSystem) CollectDisk(collector ilogtail.Collector) {
	r.CollectDiskUsage(collector)

	// disk stat
	allIoCounters, err := disk.IOCounters(r.Disks...)
	if err == nil {
		totalIoCount := disk.IOCountersStat{}
		for _, ioCount := range allIoCounters {
			totalIoCount.ReadBytes += ioCount.ReadBytes
			totalIoCount.WriteBytes += ioCount.WriteBytes
			totalIoCount.ReadCount += ioCount.ReadCount
			totalIoCount.WriteCount += ioCount.WriteCount
			totalIoCount.ReadTime += ioCount.ReadTime
			totalIoCount.WriteTime += ioCount.WriteTime
			totalIoCount.IopsInProgress += ioCount.IopsInProgress
			totalIoCount.IoTime += ioCount.IoTime

		}

		nowTime := time.Now()

		if !r.lastDiskTime.IsZero() {
			timeDeltaSec := float64(nowTime.Sub(r.lastDiskTime)) / float64(time.Second)
			r.collectOneDisk(collector, "total", timeDeltaSec, &r.lastDiskStat, &totalIoCount)
			for key, ioCount := range allIoCounters {
				if lastIOCount, ok := r.lastDiskStatAll[key]; ok {
					count := ioCount
					r.collectOneDisk(collector, key, timeDeltaSec, &lastIOCount, &count)
				}
			}
		}

		r.lastDiskTime = nowTime
		r.lastDiskStat = totalIoCount
		r.lastDiskStatAll = allIoCounters
	}
}

func (r *InputSystem) collectOneNet(collector ilogtail.Collector, name string, timeDeltaSec float64, last, now *net.IOCountersStat) {
	newLabels := r.commonLabels.Clone()
	newLabels.Append("interface", name)
	newLabels.Sort()
	labels := newLabels.String()
	r.addMetric(collector, "net_in", labels, float64(now.BytesRecv-last.BytesRecv)/timeDeltaSec)
	r.addMetric(collector, "net_out", labels, float64(now.BytesSent-last.BytesSent)/timeDeltaSec)
	r.addMetric(collector, "net_in_pkt", labels, float64(now.PacketsRecv-last.PacketsRecv)/timeDeltaSec)
	r.addMetric(collector, "net_out_pkt", labels, float64(now.PacketsSent-last.PacketsSent)/timeDeltaSec)

	deltaErrIn := now.Errin - last.Errin
	deltaErrOut := now.Errout - last.Errout
	deltaDropIn := now.Dropin - last.Dropin
	deltaDropOut := now.Dropout - last.Dropout
	deltaPacketsSent := now.PacketsSent - last.PacketsSent
	deltaPacketsRecv := now.PacketsRecv - last.PacketsRecv
	deltaErrTotal := deltaErrIn + deltaErrOut
	deltaDropTotal := deltaDropIn + deltaDropOut
	deltaPacketsTotal := deltaPacketsSent + deltaPacketsRecv
	if 0 != deltaPacketsTotal {
		r.addMetric(collector, "net_drop_util", labels, 100*float64(deltaDropTotal)/float64(deltaPacketsTotal))
		r.addMetric(collector, "net_err_util", labels, 100*float64(deltaErrTotal)/float64(deltaPacketsTotal))
		// fields["err.pkts"] = strconv.FormatInt(deltaErrTotal, 10)
		// fields["drop.pkts"] = strconv.FormatInt(deltaDropTotal, 10)
		// fields["pkts.total"] = strconv.FormatInt(int64(deltaPacketsTotal), 10)
	}
}

func (r *InputSystem) CollectNet(collector ilogtail.Collector) {
	netIoStatAll, err := net.IOCounters(true)
	if err == nil && len(netIoStatAll) > 0 {
		netIoStatTotal := net.IOCountersStat{}

		for _, netIoStat := range netIoStatAll {
			netIoStatTotal.BytesRecv += netIoStat.BytesRecv
			netIoStatTotal.BytesSent += netIoStat.BytesSent
			netIoStatTotal.Dropin += netIoStat.Dropin
			netIoStatTotal.Dropout += netIoStat.Dropout
			netIoStatTotal.Errin += netIoStat.Errin
			netIoStatTotal.Errout += netIoStat.Errout
			netIoStatTotal.Fifoin += netIoStat.Fifoin
			netIoStatTotal.Fifoout += netIoStat.Fifoout
			netIoStatTotal.PacketsRecv += netIoStat.PacketsRecv
			netIoStatTotal.PacketsSent += netIoStat.PacketsSent
		}

		nowTime := time.Now()
		if !r.lastNetTime.IsZero() {
			timeDeltaSec := float64(nowTime.Sub(r.lastNetTime)) / float64(time.Second)
			r.collectOneNet(collector, "total", timeDeltaSec, &r.lastNetStat, &netIoStatTotal)
			// collect every interface
			if len(r.lastNetStatAll) == len(netIoStatAll) {
				for i := 0; i < len(netIoStatAll); i++ {
					if netIoStatAll[i].Name == r.lastNetStatAll[i].Name {
						r.collectOneNet(collector, netIoStatAll[i].Name, timeDeltaSec, &r.lastNetStatAll[i], &netIoStatAll[i])
					}
				}
			}
		}
		r.lastNetTime = nowTime
		r.lastNetStat = netIoStatTotal
		r.lastNetStatAll = netIoStatAll
	}
}

func (r *InputSystem) CollectProtocol(collector ilogtail.Collector) {
	protoCounterStats, err := net.ProtoCounters([]string{})
	if err == nil && len(protoCounterStats) > 0 {

		nowTime := time.Now()
		retransSegField := "RetransSegs"
		totalOutSegField := "OutSegs"
		totalInSegField := "InSegs"
		if !r.lastProtoTime.IsZero() && len(protoCounterStats) == len(r.lastProtoAll) {
			for i := range protoCounterStats {
				// 只要tcp
				if protoCounterStats[i].Protocol == "tcp" {
					if len(r.lastProtoAll) <= i || r.lastProtoAll[i].Protocol != protoCounterStats[i].Protocol {
						continue
					}
					r.CollectTCPStats(collector, &protoCounterStats[i])
					deltaRetransSegs := protoCounterStats[i].Stats[retransSegField] - r.lastProtoAll[i].Stats[retransSegField]
					deltaTotalOutSegs := protoCounterStats[i].Stats[totalOutSegField] - r.lastProtoAll[i].Stats[totalOutSegField]
					deltaTotalInSegs := protoCounterStats[i].Stats[totalInSegField] - r.lastProtoAll[i].Stats[totalInSegField]

					r.addMetric(collector, "protocol_tcp_outsegs", r.commonLabelsStr, float64(deltaTotalOutSegs))
					r.addMetric(collector, "protocol_tcp_insegs", r.commonLabelsStr, float64(deltaTotalInSegs))
					r.addMetric(collector, "protocol_tcp_retran_segs", r.commonLabelsStr, float64(deltaRetransSegs))
					if deltaTotalOutSegs <= 0 {
						r.addMetric(collector, "protocol_tcp_retran_util", r.commonLabelsStr, 0.)
					} else {
						r.addMetric(collector, "protocol_tcp_retran_util", r.commonLabelsStr, 100*float64(deltaRetransSegs)/float64(deltaTotalOutSegs))
					}

				}
			}
		}
		r.lastProtoTime = nowTime
		r.lastProtoAll = protoCounterStats
	}
}

func (r *InputSystem) Collect(collector ilogtail.Collector) error {
	r.collectTime = time.Now()
	r.CollectCore(collector)
	if r.CPU {
		r.CollectCPU(collector)
	}
	if r.Mem {
		r.CollectMem(collector)
	}
	if r.Disk {
		r.CollectDisk(collector)
	}
	if r.Net {
		r.CollectNet(collector)
	}
	if r.Protocol {
		r.CollectProtocol(collector)
	}
	if r.OpenFd {
		r.CollectOpenFD(collector)
	}
	return nil
}

func init() {
	ilogtail.MetricInputs["metric_system_v2"] = func() ilogtail.MetricInput {
		return &InputSystem{
			CPUPercent:        true,
			CPU:               true,
			Mem:               true,
			Disk:              true,
			Net:               true,
			Protocol:          true,
			OpenFd:            true,
			TCP:               false,
			ExcludeDiskPath:   "^/(dev|proc|sys|var/lib/docker/.+|var/lib/kubelet/pods/.+)($|/)",
			ExcludeDiskFsType: "^(autofs|binfmt_misc|cgroup|configfs|debugfs|devpts|devtmpfs|fusectl|hugetlbfs|mqueue|overlay|proc|procfs|pstore|rpc_pipefs|securityfs|sysfs|tracefs)$",
		}
	}
}
