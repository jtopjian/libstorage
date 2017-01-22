package openstack

import (
	"time"

	gofig "github.com/akutz/gofig/types"
	"github.com/akutz/goof"

	"github.com/codedellemc/libstorage/api/context"
	"github.com/codedellemc/libstorage/api/registry"
	"github.com/codedellemc/libstorage/api/types"
	openstackdriver "github.com/codedellemc/libstorage/drivers/storage/openstack"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/volumeactions"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v1/snapshots"
	volumesv1 "github.com/gophercloud/gophercloud/openstack/blockstorage/v1/volumes"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v2/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/volumeattach"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/extensions/trusts"
)

type driver struct {
	provider             *gophercloud.ProviderClient
	clientCompute        *gophercloud.ServiceClient
	clientBlockStorage   *gophercloud.ServiceClient
	clientBlockStoragev2 *gophercloud.ServiceClient
	availabilityZone     string
	config               gofig.Config
}

func ef() goof.Fields {
	return goof.Fields{
		"provider": openstackdriver.Name,
	}
}

func eff(fields goof.Fields) map[string]interface{} {
	errFields := map[string]interface{}{
		"provider": openstackdriver.Name,
	}
	if fields != nil {
		for k, v := range fields {
			errFields[k] = v
		}
	}
	return errFields
}

func init() {
	registry.RegisterStorageDriver(openstackdriver.Name, newDriver)
}

func newDriver() types.StorageDriver {
	return &driver{}
}

func (d *driver) Name() string {
	return openstackdriver.Name
}

func (d *driver) Type(ctx types.Context) (types.StorageType, error) {
	return types.Block, nil
}

func (d *driver) Init(context types.Context, config gofig.Config) error {
	d.config = config
	fields := eff(map[string]interface{}{})
	var err error

	endpointOpts := gophercloud.EndpointOpts{}

	endpointOpts.Region = d.regionName()
	fields["region"] = endpointOpts.Region

	d.availabilityZone = d.availabilityZoneName()
	fields["availabilityZone"] = d.availabilityZone

	authOpts := d.getAuthOptions()

	fields["identityEndpoint"] = d.authURL()
	fields["userId"] = d.userID()
	fields["userName"] = d.userName()
	if d.password() == "" {
		fields["password"] = ""
	} else {
		fields["password"] = "******"
	}
	fields["tenantId"] = d.tenantID()
	fields["tenantName"] = d.tenantName()
	fields["domainId"] = d.domainID()
	fields["domainName"] = d.domainName()

	trustID := d.trustID()
	fields["trustId"] = trustID

	d.provider, err = openstack.NewClient(authOpts.IdentityEndpoint)
	if err != nil {
		return goof.WithFieldsE(fields, "error creating Keystone client", err)
	}

	if trustID != "" {
		authOptionsExt := trusts.AuthOptsExt{
			TrustID:            trustID,
			AuthOptionsBuilder: &authOpts,
		}
		err = openstack.AuthenticateV3(d.provider, authOptionsExt, endpointOpts)
	} else {
		err = openstack.Authenticate(d.provider, authOpts)
	}
	if err != nil {
		return goof.WithFieldsE(fields, "error authenticating", err)
	}

	if d.clientCompute, err = openstack.NewComputeV2(d.provider, endpointOpts); err != nil {
		return goof.WithFieldsE(fields, "error getting newComputeV2", err)
	}

	if d.clientBlockStorage, err = openstack.NewBlockStorageV1(d.provider, endpointOpts); err != nil {
		return goof.WithFieldsE(fields, "error getting newBlockStorageV1", err)
	}

	d.clientBlockStoragev2, err = openstack.NewBlockStorageV2(d.provider, endpointOpts)
	if err != nil {
		// fallback to volume v1
		context.WithFields(fields).Info("BlockStorage API V2 not available, fallback to V1")
		d.clientBlockStoragev2 = nil
	}

	context.WithFields(fields).Info("storage driver initialized")

	return nil
}

// InstanceInspect returns an instance.
func (d *driver) InstanceInspect(
	ctx types.Context,
	opts types.Store) (*types.Instance, error) {

	iid := context.MustInstanceID(ctx)
	if iid.ID != "" {
		return &types.Instance{InstanceID: iid}, nil
	}

	return nil, goof.New("Can create an instance without an instanceID")
}

func (d *driver) getAuthOptions() gophercloud.AuthOptions {
	return gophercloud.AuthOptions{
		IdentityEndpoint: d.authURL(),
		UserID:           d.userID(),
		Username:         d.userName(),
		Password:         d.password(),
		TenantID:         d.tenantID(),
		TenantName:       d.tenantName(),
		DomainID:         d.domainID(),
		DomainName:       d.domainName(),
		AllowReauth:      true,
	}
}

func (d *driver) Volumes(
	ctx types.Context,
	opts *types.VolumesOpts) ([]*types.Volume, error) {

	if d.clientBlockStoragev2 != nil {
		allPages, err := volumes.List(d.clientBlockStoragev2, nil).AllPages()
		if err != nil {
			return nil,
				goof.WithError("error listing volumes", err)
		}
		volumesOS, err := volumes.ExtractVolumes(allPages)
		if err != nil {
			return nil,
				goof.WithError("error listing volumes", err)
		}

		var volumesRet []*types.Volume
		for _, volumeOS := range volumesOS {
			volumesRet = append(volumesRet, translateVolume(
				&volumeOS, opts.Attachments))
		}

		return volumesRet, nil
	}

	allPages, err := volumesv1.List(d.clientBlockStorage, nil).AllPages()
	if err != nil {
		return nil,
			goof.WithError("error listing volumes", err)
	}
	volumesOS, err := volumesv1.ExtractVolumes(allPages)
	if err != nil {
		return nil,
			goof.WithError("error listing volumes", err)
	}

	var volumesRet []*types.Volume
	for _, volumeOS := range volumesOS {
		volumesRet = append(volumesRet, translateVolumeV1(
			&volumeOS, opts.Attachments))
	}

	return volumesRet, nil
}

func (d *driver) VolumeInspect(
	ctx types.Context,
	volumeID string,
	opts *types.VolumeInspectOpts) (*types.Volume, error) {

	fields := eff(goof.Fields{
		"volumeId": volumeID,
	})

	if volumeID == "" {
		return nil, goof.New("no volumeID specified")
	}

	if d.clientBlockStoragev2 != nil {
		volume, err := volumes.Get(d.clientBlockStoragev2, volumeID).Extract()

		if err != nil {
			return nil,
				goof.WithFieldsE(fields, "error getting volume", err)
		}

		return translateVolume(volume, opts.Attachments), nil
	}

	volume, err := volumesv1.Get(d.clientBlockStorage, volumeID).Extract()

	if err != nil {
		return nil,
			goof.WithFieldsE(fields, "error getting volume", err)
	}

	return translateVolumeV1(volume, opts.Attachments), nil
}

func translateVolumeV1(
	volume *volumesv1.Volume,
	includeAttachments types.VolumeAttachmentsTypes) *types.Volume {

	var attachments []*types.VolumeAttachment
	if includeAttachments.Requested() {
		for _, attachment := range volume.Attachments {
			libstorageAttachment := &types.VolumeAttachment{
				VolumeID:   attachment["volume_id"].(string),
				InstanceID: &types.InstanceID{ID: attachment["server_id"].(string), Driver: openstackdriver.Name},
				DeviceName: attachment["device"].(string),
				Status:     "",
			}
			attachments = append(attachments, libstorageAttachment)
		}
	}

	return &types.Volume{
		Name:             volume.Name,
		ID:               volume.ID,
		AvailabilityZone: volume.AvailabilityZone,
		Status:           volume.Status,
		Type:             volume.VolumeType,
		IOPS:             0,
		Size:             int64(volume.Size),
		Attachments:      attachments,
	}
}

func translateVolume(
	volume *volumes.Volume,
	includeAttachments types.VolumeAttachmentsTypes) *types.Volume {

	var attachments []*types.VolumeAttachment
	if includeAttachments.Requested() {
		for _, attachment := range volume.Attachments {
			libstorageAttachment := &types.VolumeAttachment{
				VolumeID:   attachment.VolumeID,
				InstanceID: &types.InstanceID{ID: attachment.ServerID, Driver: openstackdriver.Name},
				DeviceName: attachment.Device,
				Status:     "",
			}
			attachments = append(attachments, libstorageAttachment)
		}
	}

	return &types.Volume{
		Name:             volume.Name,
		ID:               volume.ID,
		AvailabilityZone: volume.AvailabilityZone,
		Status:           volume.Status,
		Type:             volume.VolumeType,
		IOPS:             0,
		Size:             int64(volume.Size),
		Attachments:      attachments,
	}
}

func (d *driver) SnapshotInspect(
	ctx types.Context,
	snapshotID string,
	opts types.Store) (*types.Snapshot, error) {

	fields := eff(map[string]interface{}{
		"snapshotId": snapshotID,
	})

	snapshot, err := snapshots.Get(d.clientBlockStorage, snapshotID).Extract()
	if err != nil {
		return nil,
			goof.WithFieldsE(fields, "error getting snapshot", err)
	}

	return translateSnapshot(snapshot), nil
}

func (d *driver) Snapshots(
	ctx types.Context,
	opts types.Store) ([]*types.Snapshot, error) {
	allPages, err := snapshots.List(d.clientBlockStorage, nil).AllPages()
	if err != nil {
		return []*types.Snapshot{},
			goof.WithError("error listing volume snapshots", err)
	}
	allSnapshots, err := snapshots.ExtractSnapshots(allPages)
	if err != nil {
		return []*types.Snapshot{},
			goof.WithError("error listing volume snapshots", err)
	}

	var libstorageSnapshots []*types.Snapshot
	for _, snapshot := range allSnapshots {
		libstorageSnapshots = append(libstorageSnapshots, translateSnapshot(&snapshot))
	}

	return libstorageSnapshots, nil
}

func translateSnapshot(snapshot *snapshots.Snapshot) *types.Snapshot {
	return &types.Snapshot{
		Name:        snapshot.Name,
		VolumeID:    snapshot.VolumeID,
		ID:          snapshot.ID,
		VolumeSize:  int64(snapshot.Size),
		StartTime:   time.Time(snapshot.CreatedAt).Unix(),
		Description: snapshot.Description,
		Status:      snapshot.Status,
	}
}

func (d *driver) VolumeSnapshot(
	ctx types.Context,
	volumeID, snapshotName string,
	opts types.Store) (*types.Snapshot, error) {

	fields := eff(map[string]interface{}{
		"snapshotName": snapshotName,
		"volumeId":     volumeID,
	})

	createOpts := snapshots.CreateOpts{
		Name:     snapshotName,
		VolumeID: volumeID,
		Force:    true,
	}

	snapshot, err := snapshots.Create(d.clientBlockStorage, createOpts).Extract()
	if err != nil {
		return nil,
			goof.WithFieldsE(fields, "error creating snapshot", err)
	}

	ctx.WithFields(fields).Info("waiting for snapshot creation to complete")

	err = snapshots.WaitForStatus(d.clientBlockStorage, snapshot.ID, "available", int(d.volumeSnapshotTimeout().Seconds()))
	if err != nil {
		return nil,
			goof.WithFieldsE(fields,
				"error waiting for snapshot creation to complete", err)
	}

	return translateSnapshot(snapshot), nil

}

func (d *driver) SnapshotRemove(
	ctx types.Context,
	snapshotID string,
	opts types.Store) error {
	resp := snapshots.Delete(d.clientBlockStorage, snapshotID)
	if resp.Err != nil {
		return goof.WithFieldsE(goof.Fields{
			"snapshotId": snapshotID}, "error removing snapshot", resp.Err)
	}

	return nil
}

func (d *driver) VolumeCreate(ctx types.Context, volumeName string,
	opts *types.VolumeCreateOpts) (*types.Volume, error) {

	return d.createVolume(ctx, volumeName, "", "", opts)
}

func (d *driver) VolumeCreateFromSnapshot(
	ctx types.Context,
	snapshotID, volumeName string,
	opts *types.VolumeCreateOpts) (*types.Volume, error) {

	return d.createVolume(ctx, volumeName, "", snapshotID, opts)
}

func (d *driver) VolumeCopy(
	ctx types.Context,
	volumeID, volumeName string,
	opts types.Store) (*types.Volume, error) {
	volume, err := d.VolumeInspect(ctx, volumeID, &types.VolumeInspectOpts{})
	if err != nil {
		return nil,
			goof.New("error getting reference volume for volume copy")
	}

	volumeCreateOpts := &types.VolumeCreateOpts{
		Type:             &volume.Type,
		AvailabilityZone: &volume.AvailabilityZone,
	}

	return d.createVolume(ctx, volumeName, volumeID, "", volumeCreateOpts)
}

func (d *driver) createVolume(
	ctx types.Context,
	volumeName string,
	volumeSourceID string,
	snapshotID string,
	opts *types.VolumeCreateOpts) (*types.Volume, error) {
	volumeType := *opts.Type
	IOPS := *opts.IOPS
	size := *opts.Size
	availabilityZone := *opts.AvailabilityZone

	fields := eff(map[string]interface{}{
		"volumeName":       volumeName,
		"snapshotId":       snapshotID,
		"volumeSourceId":   volumeSourceID,
		"volumeType":       volumeType,
		"iops":             IOPS,
		"size":             size,
		"availabilityZone": availabilityZone,
	})

	if availabilityZone == "" {
		availabilityZone = d.availabilityZone
	}

	options := &volumes.CreateOpts{
		Name:             volumeName,
		Size:             int(size),
		SnapshotID:       snapshotID,
		VolumeType:       volumeType,
		AvailabilityZone: availabilityZone,
		SourceReplica:    volumeSourceID,
	}
	if d.clientBlockStoragev2 != nil {
		volume, err := volumes.Create(d.clientBlockStoragev2, options).Extract()
		if err != nil {
			return nil,
				goof.WithFieldsE(fields, "error creating volume", err)
		}

		fields["volumeId"] = volume.ID

		ctx.WithFields(fields).Info("waiting for volume creation to complete")
		err = volumes.WaitForStatus(d.clientBlockStoragev2, volume.ID, "available", int(d.volumeCreateTimeout().Seconds()))
		if err != nil {
			return nil,
				goof.WithFieldsE(fields,
					"error waiting for volume creation to complete", err)
		}

		return translateVolume(volume, types.VolumeAttachmentsRequested), nil
	}

	volume, err := volumesv1.Create(d.clientBlockStorage, options).Extract()
	if err != nil {
		return nil,
			goof.WithFieldsE(fields, "error creating volume", err)
	}

	fields["volumeId"] = volume.ID

	ctx.WithFields(fields).Info("waiting for volume creation to complete")
	err = volumesv1.WaitForStatus(d.clientBlockStorage, volume.ID, "available", int(d.volumeCreateTimeout().Seconds()))
	if err != nil {
		return nil,
			goof.WithFieldsE(fields,
				"error waiting for volume creation to complete", err)
	}

	return translateVolumeV1(volume, types.VolumeAttachmentsRequested), nil
}

func (d *driver) VolumeRemove(
	ctx types.Context,
	volumeID string,
	opts types.Store) error {
	fields := eff(map[string]interface{}{
		"volumeId": volumeID,
	})
	if volumeID == "" {
		return goof.WithFields(fields, "volumeId is required")
	}

	if d.clientBlockStoragev2 != nil {
		res := volumes.Delete(d.clientBlockStoragev2, volumeID)
		if res.Err != nil {
			return goof.WithFieldsE(fields, "error removing volume", res.Err)
		}
	} else {
		res := volumesv1.Delete(d.clientBlockStorage, volumeID)
		if res.Err != nil {
			return goof.WithFieldsE(fields, "error removing volume", res.Err)
		}
	}

	return nil
}

func (d *driver) NextDeviceInfo(
	ctx types.Context) (*types.NextDeviceInfo, error) {
	return nil, nil
}

func (d *driver) VolumeAttach(
	ctx types.Context,
	volumeID string,
	opts *types.VolumeAttachOpts) (*types.Volume, string, error) {

	iid := context.MustInstanceID(ctx)

	fields := eff(map[string]interface{}{
		"volumeId":   volumeID,
		"instanceId": iid.ID,
	})

	if opts.Force {
		if _, err := d.VolumeDetach(ctx, volumeID, &types.VolumeDetachOpts{}); err != nil {
			return nil, "", err
		}
	}

	options := &volumeattach.CreateOpts{
		VolumeID: volumeID,
	}
	if opts.NextDevice != nil {
		options.Device = *opts.NextDevice
	}

	volumeAttach, err := volumeattach.Create(d.clientCompute, iid.ID, options).Extract()
	if err != nil {
		return nil, "", goof.WithFieldsE(
			fields, "error attaching volume", err)
	}

	ctx.WithFields(fields).Debug("waiting for volume to attach")
	volume, err := d.waitVolumeAttachStatus(ctx, volumeID, true, 30*time.Second)
	if err != nil {
		return nil, "", goof.WithFieldsE(
			fields, "error waiting for volume to attach", err)
	}

	return volume, volumeAttach.Device, nil
}

func (d *driver) VolumeDetach(
	ctx types.Context,
	volumeID string,
	opts *types.VolumeDetachOpts) (*types.Volume, error) {

	iid := context.MustInstanceID(ctx)

	fields := eff(map[string]interface{}{
		"volumeId":   volumeID,
		"instanceId": iid.ID,
	})

	if volumeID == "" {
		return nil, goof.WithFields(fields, "volumeId is required")
	}

	resp := volumeattach.Delete(d.clientCompute, iid.ID, volumeID)
	if resp.Err != nil {
		return nil, goof.WithFieldsE(fields, "error detaching volume", resp.Err)
	}

	ctx.WithFields(fields).Debug("waiting for volume to detach")
	volume, err := d.waitVolumeAttachStatus(ctx, volumeID, false, 30*time.Second)
	if err == nil {
		return volume, nil
	}

	if opts.Force && d.clientBlockStoragev2 != nil {
		resp := volumeactions.Detach(d.clientBlockStoragev2, volumeID, volumeactions.DetachOpts{})

		if resp.Err != nil {
			return nil, goof.WithFieldsE(fields, "error force detaching volume", resp.Err)
		}

		volume, err = d.waitVolumeAttachStatus(ctx, volumeID, false, 30*time.Second)
		if err != nil {
			return nil, goof.WithFieldsE(
				fields, "error waiting for volume to force detach", err)
		}

		return volume, nil
	}

	return nil, goof.WithFields(fields, "unexpected error when detaching")
}

func (d *driver) waitVolumeAttachStatus(
	ctx types.Context, volumeID string,
	attachmentNeeded bool, timeout time.Duration) (*types.Volume, error) {

	fields := eff(map[string]interface{}{
		"volumeId": volumeID,
	})

	if volumeID == "" {
		return nil, goof.WithFields(fields, "volumeId is required")
	}
	begin := time.Now()
	for time.Now().Sub(begin) < timeout {
		volume, err := d.VolumeInspect(
			ctx, volumeID, &types.VolumeInspectOpts{
				Attachments: types.VolumeAttachmentsRequested})
		if err != nil {
			return nil, goof.WithFieldsE(fields, "error getting volume when waiting", err)
		}

		if attachmentNeeded {
			if len(volume.Attachments) > 0 {
				return volume, nil
			}
		} else {
			if len(volume.Attachments) == 0 {
				return volume, nil
			}
		}

		time.Sleep(1 * time.Second)
	}

	return nil, goof.WithFields(fields, "timeout reached")
}

func (d *driver) SnapshotCopy(
	ctx types.Context,
	snapshotID, snapshotName, destinationID string,
	opts types.Store) (*types.Snapshot, error) {
	// TODO return nil, nil ?
	return nil, types.ErrNotImplemented
}

func (d *driver) authURL() string {
	return d.config.GetString("openstack.authURL")
}

func (d *driver) userID() string {
	return d.config.GetString("openstack.userID")
}

func (d *driver) userName() string {
	return d.config.GetString("openstack.userName")
}

func (d *driver) password() string {
	return d.config.GetString("openstack.password")
}

func (d *driver) tenantID() string {
	return d.config.GetString("openstack.tenantID")
}

func (d *driver) tenantName() string {
	return d.config.GetString("openstack.tenantName")
}

func (d *driver) domainID() string {
	return d.config.GetString("openstack.domainID")
}

func (d *driver) domainName() string {
	return d.config.GetString("openstack.domainName")
}

func (d *driver) regionName() string {
	return d.config.GetString("openstack.regionName")
}

func (d *driver) availabilityZoneName() string {
	return d.config.GetString("openstack.availabilityZoneName")
}

func (d *driver) trustID() string {
	return d.config.GetString("openstack.trustID")
}

func (d *driver) volumeCreateTimeout() time.Duration {
	strVal := d.config.GetString("openstack.volumeCreateTimeout")
	val, err := time.ParseDuration(strVal)

	if err != nil || val <= 0 {
		val = 10 * time.Minute
	}
	return val
}

func (d *driver) volumeSnapshotTimeout() time.Duration {
	strVal := d.config.GetString("openstack.volumeSnapshotTimeout")
	val, err := time.ParseDuration(strVal)

	if err != nil || val <= 0 {
		val = 10 * time.Minute
	}
	return val
}
