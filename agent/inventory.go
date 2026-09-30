package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Each line is one complete PowerShell statement; they are joined with "; ".
var hardwareScript = strings.Join([]string{
	`$cs=Get-CimInstance Win32_ComputerSystem`,
	`$bios=Get-CimInstance Win32_BIOS`,
	`$cpu=Get-CimInstance Win32_Processor | Select-Object -First 1`,
	`$os=Get-CimInstance Win32_OperatingSystem`,
	`$disks=@(Get-CimInstance Win32_DiskDrive | ForEach-Object { @{model=$_.Model; size_gb=[math]::Round($_.Size/1GB)} })`,
	`$nics=@(Get-CimInstance Win32_NetworkAdapterConfiguration -Filter 'IPEnabled=true' | ForEach-Object { @{description=$_.Description; mac=$_.MACAddress; ips=@($_.IPAddress)} })`,
	`@{ hardware=@{ manufacturer=$cs.Manufacturer; model=$cs.Model; serial=$bios.SerialNumber; bios_version=$bios.SMBIOSBIOSVersion; cpu=$cpu.Name; cpu_cores=$cpu.NumberOfCores; ram_gb=[math]::Round($cs.TotalPhysicalMemory/1GB,1); os_name=$os.Caption; os_version=$os.Version; disks=$disks; nics=$nics }; logged_in_user=$cs.UserName } | ConvertTo-Json -Depth 5 -Compress`,
}, "; ")

var softwareScript = strings.Join([]string{
	`$paths=@('HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*','HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*')`,
	`$null=New-PSDrive -Name HKU -PSProvider Registry -Root HKEY_USERS -ErrorAction SilentlyContinue`,
	`$paths+=@(Get-ChildItem HKU:\ -ErrorAction SilentlyContinue | Where-Object { $_.PSChildName -match '^S-1-5-21-[0-9-]+$' } | ForEach-Object { 'HKU:\'+$_.PSChildName+'\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' })`,
	`$seen=@{}`,
	`$apps=@(Get-ItemProperty $paths -ErrorAction SilentlyContinue | Where-Object { $_.DisplayName -and -not $_.SystemComponent -and -not $_.ParentKeyName } | ForEach-Object { $q=[char]34; $loc=([string]$_.InstallLocation).Trim().Trim($q); if(-not $loc -and $_.DisplayIcon){ $ic=([string]$_.DisplayIcon).Split(',')[0].Trim().Trim($q); if($ic -match '\.(exe|ico|dll)$'){ $loc=[string](Split-Path $ic -Parent -ErrorAction SilentlyContinue) } }; $src='Machine'; if($_.PSPath -like '*HKEY_USERS*'){ $src='Per-user' }; $k=[string]$_.DisplayName+'|'+[string]$_.DisplayVersion+'|'+$src; if(-not $seen.ContainsKey($k)){ $seen[$k]=1; @{name=[string]$_.DisplayName; version=[string]$_.DisplayVersion; publisher=[string]$_.Publisher; install_date=[string]$_.InstallDate; install_location=[string]$loc; source=$src} } })`,
	`try { $pk=@(Get-AppxPackage -AllUsers -ErrorAction Stop) } catch { $pk=@(Get-AppxPackage -ErrorAction SilentlyContinue) }`,
	`$store=@($pk | Where-Object { -not $_.IsFramework -and -not $_.IsResourcePackage -and $_.SignatureKind -ne 'System' } | ForEach-Object { $k=[string]$_.PackageFullName; if(-not $seen.ContainsKey($k)){ $seen[$k]=1; $pub=[string]$_.Publisher; if($pub -match 'CN=([^,]+)'){ $pub=$Matches[1].Trim([char]34) }; @{name=[string]$_.Name; version=[string]$_.Version; publisher=$pub; install_date=''; install_location=[string]$_.InstallLocation; source='Store'} } })`,
	`ConvertTo-Json -InputObject @($apps + $store) -Depth 3 -Compress`,
}, "; ")

func runPS(script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	full := "[Console]::OutputEncoding=[Text.Encoding]::UTF8; $ProgressPreference='SilentlyContinue'; " + script
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", full)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return bytes.TrimPrefix(bytes.TrimSpace(out), []byte("\xef\xbb\xbf")), nil
}

func collectInventory() ([]byte, error) {
	hwOut, err := runPS(hardwareScript)
	if err != nil {
		return nil, err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(hwOut, &payload); err != nil {
		return nil, fmt.Errorf("hardware json: %w", err)
	}
	swOut, err := runPS(softwareScript)
	if err != nil {
		return nil, err
	}
	payload["software"] = json.RawMessage(swOut)
	return json.Marshal(payload)
}

func sendInventory(creds *Creds) error {
	log.Println("collecting inventory (takes a few seconds)...")
	body, err := collectInventory()
	if err != nil {
		log.Println("inventory failed:", err)
		return err
	}
	resp, err := doAuthed(creds, "POST", "/inventory", body)
	if err != nil {
		log.Println("inventory upload failed:", err)
		return err
	}
	defer resp.Body.Close()
	log.Printf("inventory sent (%d bytes): %s", len(body), resp.Status)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server said: %s", resp.Status)
	}
	return nil
}