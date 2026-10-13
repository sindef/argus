package hostserver

import (
	"context"
	"fmt"
	"html"
	"strings"

	"argus/internal/kube"
)

func buildOVF(name string, hw kube.Hardware, guest string) string {
	esc := html.EscapeString
	var refs, disks, items strings.Builder
	item := func(fields ...string) {
		items.WriteString("   <Item>\n")
		for i := 0; i+1 < len(fields); i += 2 {
			fmt.Fprintf(&items, "    <rasd:%s>%s</rasd:%s>\n", fields[i], esc(fields[i+1]), fields[i])
		}
		items.WriteString("   </Item>\n")
	}
	cps := max(hw.CoresPerSocket, 1)
	fmt.Fprintf(&items, "   <Item>\n    <rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits>\n    <rasd:ElementName>%d virtual CPU(s)</rasd:ElementName>\n    <rasd:InstanceID>1</rasd:InstanceID>\n    <rasd:ResourceType>3</rasd:ResourceType>\n    <rasd:VirtualQuantity>%d</rasd:VirtualQuantity>\n    <vmw:CoresPerSocket ovf:required=\"false\">%d</vmw:CoresPerSocket>\n   </Item>\n", hw.Cores, hw.Cores, cps)
	item("AllocationUnits", "byte * 2^20", "ElementName", fmt.Sprintf("%d MB of memory", hw.MemoryMiB), "InstanceID", "2", "ResourceType", "4", "VirtualQuantity", fmt.Sprint(hw.MemoryMiB))
	item("Address", "0", "ElementName", "SCSI controller 0", "InstanceID", "3", "ResourceSubType", "lsilogicsas", "ResourceType", "6")
	for i, d := range hw.Disks {
		file := name + "-" + d.Name + ".vmdk"
		fmt.Fprintf(&refs, "  <File ovf:id=\"file%d\" ovf:href=\"%s\"/>\n", i+1, esc(file))
		fmt.Fprintf(&disks, "  <Disk ovf:diskId=\"vmdisk%d\" ovf:fileRef=\"file%d\" ovf:capacity=\"%d\" ovf:capacityAllocationUnits=\"byte * 2^30\" ovf:format=\"http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized\"/>\n", i+1, i+1, d.SizeGiB)
		item("AddressOnParent", fmt.Sprint(i), "ElementName", "Hard disk "+fmt.Sprint(i+1), "HostResource", fmt.Sprintf("ovf:/disk/vmdisk%d", i+1), "InstanceID", fmt.Sprint(10+i), "Parent", "3", "ResourceType", "17")
	}
	for i, n := range hw.NICs {
		item("Address", n.MAC, "AutomaticAllocation", "true", "Connection", "VM Network", "ElementName", "Network adapter "+fmt.Sprint(i+1), "InstanceID", fmt.Sprint(30+i), "ResourceSubType", "E1000", "ResourceType", "10")
	}
	if strings.HasPrefix(hw.Firmware, "uefi") {
		items.WriteString("   <vmw:Config ovf:required=\"false\" vmw:key=\"firmware\" vmw:value=\"efi\"/>\n")
	}
	osType := "otherGuest64"
	if strings.Contains(strings.ToLower(guest), "windows") {
		osType = "windows9Server64Guest"
	} else if guest != "" {
		osType = "otherLinux64Guest"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1" xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData" xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData" xmlns:vmw="http://www.vmware.com/schema/ovf" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
 <References>
%s </References>
 <DiskSection>
  <Info>Virtual disks</Info>
%s </DiskSection>
 <NetworkSection>
  <Info>Networks</Info>
  <Network ovf:name="VM Network"><Description>VM network</Description></Network>
 </NetworkSection>
 <VirtualSystem ovf:id="%s">
  <Info>A virtual machine exported from Argus</Info>
  <Name>%s</Name>
  <OperatingSystemSection ovf:id="1" vmw:osType="%s"><Info>Guest operating system</Info></OperatingSystemSection>
  <VirtualHardwareSection>
   <Info>Virtual hardware</Info>
   <System>
    <vssd:ElementName>Virtual Hardware Family</vssd:ElementName>
    <vssd:InstanceID>0</vssd:InstanceID>
    <vssd:VirtualSystemIdentifier>%s</vssd:VirtualSystemIdentifier>
    <vssd:VirtualSystemType>vmx-14</vssd:VirtualSystemType>
   </System>
%s  </VirtualHardwareSection>
 </VirtualSystem>
</Envelope>
`, refs.String(), disks.String(), esc(name), esc(name), osType, esc(name), items.String())
}

func (l localOps) OVF(ctx context.Context, ns, name string) (string, error) {
	hw, err := l.s.kube.Hardware(ctx, ns, name)
	if err != nil {
		return "", err
	}
	vm, _ := l.s.kube.GetVM(ctx, ns, name)
	return buildOVF(name, hw, vm.OS), nil
}
