package store

import "context"

func (s *Store) DeploymentDomain(ctx context.Context) (string, error) {
	row, err := s.db.DeploymentDomain.Get(ctx, 1)
	if err != nil {
		return "", err
	}
	return row.Region, nil
}
