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
	`$vols=@(Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=2 OR DriveType=3' | Where-Object { $_.Size } | ForEach-Object { @{drive=$_.DeviceID; label=[string]$_.VolumeName; size_gb=[math]::Round($_.Size/1GB); free_gb=[math]::Round($_.FreeSpace/1GB); type=$(if($_.DriveType -eq 2){'Removable'}else{'Fixed'})} })`,
	`$nics=@(Get-CimInstance Win32_NetworkAdapterConfiguration -Filter 'IPEnabled=true' | ForEach-Object { @{description=$_.Description; mac=$_.MACAddress; ips=@($_.IPAddress)} })`,
	`@{ hardware=@{ manufacturer=$cs.Manufacturer; model=$cs.Model; serial=$bios.SerialNumber; bios_version=$bios.SMBIOSBIOSVersion; cpu=$cpu.Name; cpu_cores=$cpu.NumberOfCores; ram_gb=[math]::Round($cs.TotalPhysicalMemory/1GB,1); os_name=$os.Caption; os_version=$os.Version; disks=$disks; volumes=$vols; nics=$nics }; logged_in_user=$cs.UserName } | ConvertTo-Json -Depth 5 -Compress`,
}, "; ")

var softwareScript = strings.Join([]string{
	`$paths=@('HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*','HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*')`,
	`$null=New-PSDrive -Name HKU -PSProvider Registry -Root HKEY_USERS -ErrorAction SilentlyContinue`,
	`$paths+=@(Get-ChildItem HKU:\ -ErrorAction SilentlyContinue | Where-Object { $_.PSChildName -match '^S-1-5-21-[0-9-]+$' } | ForEach-Object { 'HKU:\'+$_.PSChildName+'\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*' })`,
	`$seen=@{}`,
	`$apps=@(Get-ItemProperty $paths -ErrorAction SilentlyContinue | Where-Object { $_.DisplayName -and -not $_.SystemComponent -and -not $_.ParentKeyName } | ForEach-Object { $q=[char]34; $loc=([string]$_.InstallLocation).Trim().Trim($q); if(-not $loc -and $_.DisplayIcon){ $ic=([string]$_.DisplayIcon).Split(',')[0].Trim().Trim($q); if($ic -match '\.(exe|ico|dll)$'){ $loc=[string](Split-Path $ic -Parent -ErrorAction SilentlyContinue) } }; $src='Machine'; if($_.PSPath -like '*HKEY_USERS*'){ $src='Per-user' }; $k=[string]$_.DisplayName+'|'+[string]$_.DisplayVersion+'|'+$src; if(-not $seen.ContainsKey($k)){ $seen[$k]=1; @{name=[string]$_.DisplayName; version=[string]$_.DisplayVersion; publisher=[string]$_.Publisher; install_date=[string]$_.InstallDate; install_location=[string]$loc; drive=$(if($loc -match '^[A-Za-z]:'){ $loc.Substring(0,2).ToUpper() }else{ '' }); source=$src} } })`,
	`try { $pk=@(Get-AppxPackage -AllUsers -ErrorAction Stop) } catch { $pk=@(Get-AppxPackage -ErrorAction SilentlyContinue) }`,
	`$store=@($pk | Where-Object { -not $_.IsFramework -and -not $_.IsResourcePackage -and $_.SignatureKind -ne 'System' } | ForEach-Object { $k=[string]$_.PackageFullName; if(-not $seen.ContainsKey($k)){ $seen[$k]=1; $pub=[string]$_.Publisher; if($pub -match 'CN=([^,]+)'){ $pub=$Matches[1].Trim([char]34) }; $il=[string]$_.InstallLocation; @{name=[string]$_.Name; version=[string]$_.Version; publisher=$pub; install_date=''; install_location=$il; drive=$(if($il -match '^[A-Za-z]:'){ $il.Substring(0,2).ToUpper() }else{ '' }); source='Store'} } })`,
	`$sysd=$env:SystemDrive`,
	`$vl=@(Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=2 OR DriveType=3' | Where-Object { $_.DeviceID -ne $sysd -and $_.Size } | ForEach-Object { $_.DeviceID })`,
	`$kn=@(@($apps)+@($store) | ForEach-Object { ([string]$_.install_location).Trim().TrimEnd('\').ToLower() } | Where-Object { $_ })`,
	`$skip=@('$recycle.bin','system volume information','windows','users','programdata','recovery','documents and settings','perflogs','windows.old','$windows.~bt','msocache','config.msi')`,
	`$cont=@('program files','program files (x86)','games','apps','software','programs','applications')`,
	`$cands=@()`,
	`foreach($d in $vl){ foreach($t in @(Get-ChildItem -LiteralPath ($d+'\') -Directory -Force -ErrorAction SilentlyContinue)){ $tl=$t.Name.ToLower(); if($skip -contains $tl){ continue }; if($cont -contains $tl){ foreach($s in @(Get-ChildItem -LiteralPath $t.FullName -Directory -Force -ErrorAction SilentlyContinue)){ $cands+=[pscustomobject]@{dir=$s;dep=2;drive=$d} } } else { $cands+=[pscustomobject]@{dir=$t;dep=1;drive=$d} } } }`,
	`$extra=@($cands | ForEach-Object { $dl=$_.dir.FullName.TrimEnd('\').ToLower(); $hit=$false; foreach($k in $kn){ if($k -eq $dl -or $k.StartsWith($dl+'\') -or $dl.StartsWith($k+'\')){ $hit=$true; break } }; if(-not $hit){ $exe=Get-ChildItem -LiteralPath $_.dir.FullName -Filter *.exe -File -Recurse -Depth $_.dep -ErrorAction SilentlyContinue | Select-Object -First 1; if($exe){ $vi=$exe.VersionInfo; @{name=[string]$_.dir.Name; version=[string]$vi.ProductVersion; publisher=[string]$vi.CompanyName; install_date=''; install_location=[string]$_.dir.FullName; drive=[string]$_.drive; source='Drive scan'} } } })`,
	`ConvertTo-Json -InputObject (@($apps) + @($store) + @($extra)) -Depth 3 -Compress`,
}, "; ")

func runPS(script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	// Licences + signed-in accounts (accounts.go). Never fails the inventory.
	accts, lics := collectAccounts()
	if b, err := json.Marshal(accts); err == nil {
		payload["accounts"] = b
	}
	if b, err := json.Marshal(lics); err == nil {
		payload["licenses"] = b
	}
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