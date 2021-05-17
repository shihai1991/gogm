package gogm

import (
	"context"
	"errors"
	"fmt"
	"github.com/adam-hanna/arrayOperations"
	"github.com/cornelk/hashmap"
	dsl "github.com/mindstand/go-cypherdsl"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j"
)

func resultToStringArrV3(result [][]interface{}) ([]string, error) {
	if result == nil {
		return nil, errors.New("result is nil")
	}

	var _result []string

	for _, res := range result {
		val := res
		// nothing to parse
		if val == nil || len(val) == 0 {
			continue
		}

		str, ok := val[0].(string)
		if !ok {
			return nil, fmt.Errorf("unable to parse [%T] to string. Value is %v: %w", val[0], val[0], ErrInternal)
		}

		_result = append(_result, str)
	}

	return _result, nil
}

//drops all known indexes
func dropAllIndexesAndConstraintsV3(gogm *Gogm) error {
	sess, err := gogm.NewSessionV2(SessionConfig{
		AccessMode: neo4j.AccessModeWrite,
	})
	if err != nil {
		return err
	}
	defer sess.Close()

	ctx := context.Background()

	return sess.ManagedTransaction(ctx, func(tx TransactionV2) error {
		vals, _, err := tx.QueryRaw(ctx, "CALL db.constraints", nil)
		if err != nil {
			return err
		}

		constraints, err := resultToStringArrV3(vals)
		if err != nil {
			return err
		}

		//if there is anything, get rid of it
		if len(constraints) != 0 {
			for _, constraint := range constraints {
				gogm.logger.Debugf("dropping constraint '%s'", constraint)
				_, _, err := tx.QueryRaw(ctx, fmt.Sprintf("DROP %s", constraint), nil)
				if err != nil {
					return tx.RollbackWithError(ctx, err)
				}
			}
		}

		vals, _, err = tx.QueryRaw(ctx, "CALL db.indexes()", nil)
		if err != nil {
			return err
		}

		indexes, err := resultToStringArrV3(vals)
		if err != nil {
			return err
		}

		//if there is anything, get rid of it
		if len(indexes) != 0 {
			for _, index := range indexes {
				if len(index) == 0 {
					return errors.New("invalid index config")
				}

				_, _, err := tx.QueryRaw(ctx, fmt.Sprintf("DROP %s", index), nil)
				if err != nil {
					return tx.RollbackWithError(ctx, err)
				}
			}

			return tx.Commit(ctx)
		} else {
			return nil
		}
	})
}

//creates all indexes
func createAllIndexesAndConstraintsV3(gogm *Gogm, mappedTypes *hashmap.HashMap) error {
	sess, err := gogm.NewSessionV2(SessionConfig{
		AccessMode: neo4j.AccessModeWrite,
	})
	if err != nil {
		return err
	}
	defer sess.Close()

	ctx := context.Background()

	//validate that we have to do anything
	if mappedTypes == nil || mappedTypes.Len() == 0 {
		return errors.New("must have types to map")
	}

	numIndexCreated := 0

	return sess.ManagedTransaction(ctx, func(tx TransactionV2) error {
		//index and/or create unique constraints wherever necessary
		//for node, structConfig := range mappedTypes{
		for nodes := range mappedTypes.Iter() {
			node := nodes.Key.(string)
			structConfig := nodes.Value.(structDecoratorConfig)
			if structConfig.Fields == nil || len(structConfig.Fields) == 0 {
				continue
			}

			var indexFields []string

			for _, config := range structConfig.Fields {
				//pk is a special unique key
				if config.PrimaryKey || config.Unique {
					numIndexCreated++

					cyp, err := dsl.QB().Create(dsl.NewConstraint(&dsl.ConstraintConfig{
						Unique: true,
						Name:   node,
						Type:   structConfig.Label,
						Field:  config.Name,
					})).ToCypher()
					if err != nil {
						return err
					}

					_, _, err = tx.QueryRaw(ctx, cyp, nil)
					if err != nil {
						return tx.RollbackWithError(ctx, err)
					}
				} else if config.Index {
					indexFields = append(indexFields, config.Name)
				}
			}

			//create composite index
			if len(indexFields) > 0 {
				numIndexCreated++
				cyp, err := dsl.QB().Create(dsl.NewIndex(&dsl.IndexConfig{
					Type:   structConfig.Label,
					Fields: indexFields,
				})).ToCypher()
				if err != nil {
					return err
				}

				_, _, err = tx.QueryRaw(ctx, cyp, nil)
				if err != nil {
					return tx.RollbackWithError(ctx, err)
				}
			}
		}

		gogm.logger.Debugf("created (%v) indexes", numIndexCreated)

		return tx.Commit(ctx)
	})
}

//verifies all indexes
func verifyAllIndexesAndConstraintsV3(gogm *Gogm, mappedTypes *hashmap.HashMap) error {
	sess, err := gogm.NewSessionV2(SessionConfig{
		AccessMode: neo4j.AccessModeRead,
	})
	if err != nil {
		return err
	}
	defer sess.Close()

	ctx := context.Background()

	//validate that we have to do anything
	if mappedTypes == nil || mappedTypes.Len() == 0 {
		return errors.New("must have types to map")
	}

	var constraints []string
	var indexes []string

	//build constraint strings
	for nodes := range mappedTypes.Iter() {
		node := nodes.Key.(string)
		structConfig := nodes.Value.(structDecoratorConfig)

		if structConfig.Fields == nil || len(structConfig.Fields) == 0 {
			continue
		}

		fields := []string{}

		for _, config := range structConfig.Fields {

			if config.PrimaryKey || config.Unique {
				t := fmt.Sprintf("CONSTRAINT ON (%s:%s) ASSERT %s.%s IS UNIQUE", node, structConfig.Label, node, config.Name)
				constraints = append(constraints, t)

				indexes = append(indexes, fmt.Sprintf("INDEX ON :%s(%s)", structConfig.Label, config.Name))

			} else if config.Index {
				fields = append(fields, config.Name)
			}
		}

		f := "("
		for _, field := range fields {
			f += field
		}

		f += ")"

		indexes = append(indexes, fmt.Sprintf("INDEX ON :%s%s", structConfig.Label, f))

	}

	//get whats there now
	foundResult, _, err := sess.QueryRaw(ctx, "CALL db.constraints", nil)
	if err != nil {
		return err
	}

	foundConstraints, err := resultToStringArrV3(foundResult)
	if err != nil {
		return err
	}

	foundInxdexResult, _, err := sess.QueryRaw(ctx, "CALL db.indexes()", nil)
	if err != nil {
		return err
	}

	foundIndexes, err := resultToStringArrV3(foundInxdexResult)
	if err != nil {
		return err
	}

	//verify from there
	delta, found := arrayOperations.Difference(foundIndexes, indexes)
	if !found {
		return fmt.Errorf("found differences in remote vs ogm for found indexes, %v", delta)
	}

	gogm.logger.Debugf("%+v", delta)

	var founds []string

	for _, constraint := range foundConstraints {
		founds = append(founds, constraint)
	}

	delta, found = arrayOperations.Difference(founds, constraints)
	if !found {
		return fmt.Errorf("found differences in remote vs ogm for found constraints, %v", delta)
	}

	gogm.logger.Debugf("%+v", delta)

	return nil
}
