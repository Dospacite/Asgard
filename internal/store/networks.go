package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) CreateManagedNetwork(ctx context.Context, item ManagedNetwork) error {
	now := Now()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO managed_networks(id,slug,name,docker_name,description,driver,internal,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, item.ID, item.Slug, item.Name, item.DockerName, item.Description, item.Driver, item.Internal, now, now)
	return err
}

func (s *Store) GetManagedNetwork(ctx context.Context, idOrSlug string) (ManagedNetwork, error) {
	var item ManagedNetwork
	var internal int
	var created, updated string
	err := s.DB.QueryRowContext(ctx, `SELECT id,slug,name,docker_name,description,driver,internal,created_at,updated_at FROM managed_networks WHERE id=? OR slug=?`, idOrSlug, idOrSlug).
		Scan(&item.ID, &item.Slug, &item.Name, &item.DockerName, &item.Description, &item.Driver, &internal, &created, &updated)
	if err != nil {
		return item, err
	}
	item.Internal = internal != 0
	item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
	item.Members, err = s.ListNetworkMembers(ctx, item.ID)
	return item, err
}

func (s *Store) ListManagedNetworks(ctx context.Context) ([]ManagedNetwork, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,slug,name,docker_name,description,driver,internal,created_at,updated_at FROM managed_networks ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	items := []ManagedNetwork{}
	for rows.Next() {
		var item ManagedNetwork
		var internal int
		var created, updated string
		if err := rows.Scan(&item.ID, &item.Slug, &item.Name, &item.DockerName, &item.Description, &item.Driver, &internal, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		item.Internal = internal != 0
		item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range items {
		items[index].Members, err = s.ListNetworkMembers(ctx, items[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

func (s *Store) ListNetworkMembers(ctx context.Context, networkID string) ([]NetworkMember, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT nm.network_id,nm.project_id,p.slug,p.name,nm.service_id,s.name,nm.alias,
		       COALESCE(rt.state,''),COALESCE(rt.docker_id,''),COALESCE(rt.docker_name,''),nm.created_at
		FROM network_memberships nm
		JOIN projects p ON p.id=nm.project_id
		JOIN services s ON s.id=nm.service_id
		LEFT JOIN runtime_containers rt ON rt.service_id=nm.service_id AND rt.active=1
		WHERE nm.network_id=?
		ORDER BY p.name COLLATE NOCASE,s.name COLLATE NOCASE`, networkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NetworkMember{}
	for rows.Next() {
		var item NetworkMember
		var created string
		if err := rows.Scan(&item.NetworkID, &item.ProjectID, &item.ProjectSlug, &item.ProjectName, &item.ServiceID, &item.ServiceName, &item.Alias, &item.RuntimeState, &item.DockerID, &item.DockerName, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = parseTime(created)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ListServiceNetworks(ctx context.Context, serviceID string) ([]NetworkRef, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT n.id,n.slug,n.name,n.docker_name,nm.alias,n.internal
		FROM network_memberships nm
		JOIN managed_networks n ON n.id=nm.network_id
		WHERE nm.service_id=?
		ORDER BY n.name COLLATE NOCASE`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []NetworkRef{}
	for rows.Next() {
		var item NetworkRef
		var internal int
		if err := rows.Scan(&item.ID, &item.Slug, &item.Name, &item.DockerName, &item.Alias, &internal); err != nil {
			return nil, err
		}
		item.Internal = internal != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) AddNetworkMember(ctx context.Context, networkID, projectID, serviceID, alias string) error {
	var actualProjectID string
	if err := s.DB.QueryRowContext(ctx, `SELECT project_id FROM services WHERE id=?`, serviceID).Scan(&actualProjectID); err != nil {
		return err
	}
	if actualProjectID != projectID {
		return errors.New("service does not belong to the selected project")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO network_memberships(network_id,project_id,service_id,alias,created_at) VALUES(?,?,?,?,?)`, networkID, projectID, serviceID, alias, Now())
	return err
}

func (s *Store) GetNetworkMember(ctx context.Context, networkID, serviceID string) (NetworkMember, error) {
	var item NetworkMember
	var created string
	err := s.DB.QueryRowContext(ctx, `
		SELECT nm.network_id,nm.project_id,p.slug,p.name,nm.service_id,s.name,nm.alias,
		       COALESCE(rt.state,''),COALESCE(rt.docker_id,''),COALESCE(rt.docker_name,''),nm.created_at
		FROM network_memberships nm
		JOIN projects p ON p.id=nm.project_id
		JOIN services s ON s.id=nm.service_id
		LEFT JOIN runtime_containers rt ON rt.service_id=nm.service_id AND rt.active=1
		WHERE nm.network_id=? AND nm.service_id=?`, networkID, serviceID).
		Scan(&item.NetworkID, &item.ProjectID, &item.ProjectSlug, &item.ProjectName, &item.ServiceID, &item.ServiceName, &item.Alias, &item.RuntimeState, &item.DockerID, &item.DockerName, &created)
	item.CreatedAt = parseTime(created)
	return item, err
}

func (s *Store) RemoveNetworkMember(ctx context.Context, networkID, serviceID string) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM network_memberships WHERE network_id=? AND service_id=?`, networkID, serviceID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteManagedNetwork(ctx context.Context, networkID string) error {
	var members int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM network_memberships WHERE network_id=?`, networkID).Scan(&members); err != nil {
		return err
	}
	if members > 0 {
		return fmt.Errorf("network still has %d attached service(s)", members)
	}
	result, err := s.DB.ExecContext(ctx, `DELETE FROM managed_networks WHERE id=?`, networkID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}
