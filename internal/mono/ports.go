package mono

import (
	"fmt"
	"hash/fnv"
)

const (
	BasePort             = 19000
	PortRangePerWorktree = 10
	MaxPort              = 65535
	MaxPortSlots         = (MaxPort - BasePort) / PortRangePerWorktree
)

type Allocation struct {
	Service       string
	ContainerPort int
	HostPort      int
}

func Allocate(envName string, servicePorts map[string][]int) []Allocation {
	h := fnv.New32a()
	h.Write([]byte(envName))
	slot := int(h.Sum32()) % MaxPortSlots
	basePort := BasePort + (slot * PortRangePerWorktree)

	var allocations []Allocation
	usedPorts := make(map[int]bool)
	portIndex := 0

	for service, ports := range servicePorts {
		for _, containerPort := range ports {
			hostPort := basePort + (containerPort % PortRangePerWorktree)
			for usedPorts[hostPort] {
				hostPort = basePort + portIndex
				portIndex++
			}
			usedPorts[hostPort] = true
			allocations = append(allocations, Allocation{
				Service:       service,
				ContainerPort: containerPort,
				HostPort:      hostPort,
			})
		}
	}

	return allocations
}

func (a Allocation) String() string {
	return fmt.Sprintf("%s:%d -> %d", a.Service, a.ContainerPort, a.HostPort)
}

func AllocationsToMap(allocations []Allocation) map[string]int {
	result := make(map[string]int)
	for _, a := range allocations {
		result[a.Service] = a.HostPort
	}
	return result
}
