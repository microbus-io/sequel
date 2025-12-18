package serviceapi

import (
	"context"
)

// Client is a stub.
type Client struct {
}

func NewClient(caller any) Client {
	return Client{}
}

func (_c Client) Create(ctx context.Context, obj *Obj) (objKey ObjKey, err error) {
	return objKey, nil
}

func (_c Client) Store(ctx context.Context, obj *Obj) (stored bool, err error) {
	return false, nil
}

func (_c Client) MustStore(ctx context.Context, obj *Obj) (err error) {
	return nil
}

func (_c Client) Revise(ctx context.Context, obj *Obj) (revised bool, err error) {
	return false, nil
}

func (_c Client) MustRevise(ctx context.Context, obj *Obj) (err error) {
	return nil
}

func (_c Client) Delete(ctx context.Context, objKey ObjKey) (deleted bool, err error) {
	return false, nil
}

func (_c Client) MustDelete(ctx context.Context, objKey ObjKey) (err error) {
	return nil
}

func (_c Client) List(ctx context.Context, query Query) (objs []*Obj, totalCount int, err error) {
	return nil, 0, nil
}

func (_c Client) Lookup(ctx context.Context, query Query) (obj *Obj, found bool, err error) {
	return nil, false, nil
}

func (_c Client) MustLookup(ctx context.Context, query Query) (obj *Obj, err error) {
	return nil, nil
}

func (_c Client) Load(ctx context.Context, objKey ObjKey) (obj *Obj, found bool, err error) {
	return nil, false, nil
}

func (_c Client) MustLoad(ctx context.Context, objKey ObjKey) (obj *Obj, err error) {
	return nil, nil
}

func (_c Client) BulkLoad(ctx context.Context, objKeys []ObjKey) (books []*Obj, err error) {
	return nil, nil
}

func (_c Client) BulkDelete(ctx context.Context, objKeys []ObjKey) (deletedKeys []*ObjKey, err error) {
	return nil, nil
}

func (_c Client) BulkCreate(ctx context.Context, objs []*Obj) (objKeys []ObjKey, err error) {
	return nil, nil
}

func (_c Client) BulkStore(ctx context.Context, objs []*Obj) (storedKeys []ObjKey, err error) {
	return nil, nil
}

func (_c Client) BulkRevise(ctx context.Context, objs []*Obj) (revisedKeys []ObjKey, err error) {
	return nil, nil
}

func (_c Client) Purge(ctx context.Context, query Query) (deletedKeys []*ObjKey, err error) {
	return nil, nil
}

func (_c Client) Count(ctx context.Context, query Query) (count int, err error) {
	return 0, nil
}
