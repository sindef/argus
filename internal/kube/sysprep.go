package kube

import (
	"bytes"
	"cmp"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type WindowsGuest struct {
	ComputerName   string
	AdminPassword  string
	TimeZone       string
	ProductKey     string
	Domain         string
	DomainUser     string
	DomainPassword string
	Commands       []string
	VirtIO         bool
}

var validComputerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,14}$`)

func (g WindowsGuest) Validate() error {
	switch {
	case !validComputerName.MatchString(g.ComputerName):
		return errors.New("computer name must be 1-15 letters, digits or dashes")
	case g.AdminPassword == "":
		return errors.New("administrator password required")
	case g.Domain != "" && (g.DomainUser == "" || g.DomainPassword == ""):
		return errors.New("joining a domain needs a user and password")
	}
	return nil
}

func x(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

const shell = `name="%s" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS"`

func (g WindowsGuest) Unattend() string {
	var spec strings.Builder
	fmt.Fprintf(&spec, "  <component "+shell+">\n   <ComputerName>%s</ComputerName>\n   <TimeZone>%s</TimeZone>\n", "Microsoft-Windows-Shell-Setup", x(g.ComputerName), x(cmp.Or(g.TimeZone, "UTC")))
	if g.ProductKey != "" {
		fmt.Fprintf(&spec, "   <ProductKey>%s</ProductKey>\n", x(g.ProductKey))
	}
	spec.WriteString("  </component>\n")
	if g.Domain != "" {
		fmt.Fprintf(&spec, "  <component "+shell+">\n   <Identification>\n    <Credentials><Domain>%s</Domain><Username>%s</Username><Password>%s</Password></Credentials>\n    <JoinDomain>%s</JoinDomain>\n   </Identification>\n  </component>\n",
			"Microsoft-Windows-UnattendedJoin", x(g.Domain), x(g.DomainUser), x(g.DomainPassword), x(g.Domain))
	}
	if len(g.Commands) > 0 {
		fmt.Fprintf(&spec, "  <component "+shell+">\n   <RunSynchronous>\n", "Microsoft-Windows-Deployment")
		for i, c := range g.Commands {
			fmt.Fprintf(&spec, "    <RunSynchronousCommand wcm:action=\"add\"><Order>%d</Order><Path>%s</Path></RunSynchronousCommand>\n", i+1, x(c))
		}
		spec.WriteString("   </RunSynchronous>\n  </component>\n")
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
 <settings pass="specialize">
%s </settings>
 <settings pass="oobeSystem">
  <component `+shell+`>
   <OOBE><HideEULAPage>true</HideEULAPage><HideOnlineAccountScreens>true</HideOnlineAccountScreens><HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE><ProtectYourPC>3</ProtectYourPC><SkipMachineOOBE>true</SkipMachineOOBE><SkipUserOOBE>true</SkipUserOOBE></OOBE>
   <UserAccounts><AdministratorPassword><Value>%s</Value><PlainText>true</PlainText></AdministratorPassword></UserAccounts>
  </component>
 </settings>
</unattend>
`, spec.String(), "Microsoft-Windows-Shell-Setup", x(g.AdminPassword))
}
