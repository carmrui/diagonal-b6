package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"

	"diagonal.works/b6"
	"diagonal.works/b6/geojson"
	"diagonal.works/b6/geometry"
	"diagonal.works/b6/ingest"
	pb "diagonal.works/b6/proto"

	"github.com/golang/geo/s2"
	"google.golang.org/protobuf/proto"
)

func ToProto(v interface{}) (*pb.NodeProto, error) {
	if v == nil {
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_NilValue{},
				},
			},
		}, nil
	}
	switch v := v.(type) {
	case bool:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_BoolValue{
						BoolValue: v,
					},
				},
			},
		}, nil
	case int:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_IntValue{
						IntValue: int64(v),
					},
				},
			},
		}, nil
	case int8:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_IntValue{
						IntValue: int64(v),
					},
				},
			},
		}, nil
	case int16:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_IntValue{
						IntValue: int64(v),
					},
				},
			},
		}, nil
	case int32:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_IntValue{
						IntValue: int64(v),
					},
				},
			},
		}, nil
	case int64:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_IntValue{
						IntValue: int64(v),
					},
				},
			},
		}, nil
	case float64:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_FloatValue{
						FloatValue: v,
					},
				},
			},
		}, nil
	case IntNumber:
		return ToProto(int(v))
	case FloatNumber:
		return ToProto(float64(v))
	case string:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_StringValue{
						StringValue: v,
					},
				},
			},
		}, nil
	case b6.FeatureID:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_FeatureIDValue{
						FeatureIDValue: b6.NewProtoFromFeatureID(v),
					},
				},
			},
		}, nil
	// Order is significant here, as Features will also implement
	// one of the geometry types below.
	case b6.Feature:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_FeatureValue{
						FeatureValue: b6.NewProtoFromFeature(v),
					},
				},
			},
		}, nil
	case b6.Point:
		ll := s2.LatLngFromPoint(v.Point())
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_PointValue{
						PointValue: b6.S2LatLngToPointProto(ll),
					},
				},
			},
		}, nil
	case b6.Path:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_PathValue{
						PathValue: b6.NewPolylineProto(v.Polyline()),
					},
				},
			},
		}, nil
	case b6.Area:
		polygons := make(geometry.MultiPolygon, v.Len())
		for i := 0; i < v.Len(); i++ {
			polygons[i] = v.Polygon(i)
		}
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_AreaValue{
						AreaValue: b6.NewMultiPolygonProto(polygons),
					},
				},
			},
		}, nil
	case Collection:
		return collectionToProto(v)
	case b6.Tag:
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_TagValue{
						TagValue: &pb.TagProto{
							Key:   v.Key,
							Value: v.Value,
						},
					},
				},
			},
		}, nil
	case Pair:
		fp, err := ToProto(v.First())
		if err != nil {
			return nil, err
		}
		sp, err := ToProto(v.Second())
		if err != nil {
			return nil, err
		}
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_PairValue{
						PairValue: &pb.PairProto{
							First:  fp.GetLiteral(),
							Second: sp.GetLiteral(),
						},
					},
				},
			},
		}, nil
	case geojson.GeoJSON:
		marshalled, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		var buffer bytes.Buffer
		writer := gzip.NewWriter(&buffer)
		_, err = writer.Write(marshalled)
		if err != nil {
			return nil, err
		}
		if err = writer.Close(); err != nil {
			return nil, err
		}
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_GeoJSONValue{
						GeoJSONValue: buffer.Bytes(),
					},
				},
			},
		}, nil
	case *pb.QueryProto:
		n := &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_QueryValue{
						QueryValue: &pb.QueryProto{},
					},
				},
			},
		}
		proto.Merge(n.GetLiteral().GetQueryValue(), v)
		return n, nil
	case b6.Query:
		if q, err := v.ToProto(); err == nil {
			return ToProto(q) // Wrap in a NodeProto
		} else {
			return nil, err
		}
	case ingest.AppliedChange:
		c := &pb.AppliedChangeProto{
			Original: make([]*pb.FeatureIDProto, 0, len(v)),
			Modified: make([]*pb.FeatureIDProto, 0, len(v)),
		}
		for original, modified := range v {
			c.Original = append(c.Original, b6.NewProtoFromFeatureID(original))
			c.Modified = append(c.Modified, b6.NewProtoFromFeatureID(modified))
		}
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_AppliedChangeValue{
						AppliedChangeValue: c,
					},
				},
			},
		}, nil
	case Callable:
		// TODO: this could be better - in particular, we'd want to return an expression, not
		// the debug representation we use here.
		return &pb.NodeProto{
			Node: &pb.NodeProto_Literal{
				Literal: &pb.LiteralNodeProto{
					Value: &pb.LiteralNodeProto_StringValue{
						StringValue: v.String(),
					},
				},
			},
		}, nil
	default:
		panic(fmt.Sprintf("can't convert %T to proto", v))
	}
}

func collectionToProto(collection Collection) (*pb.NodeProto, error) {
	i := collection.Begin()
	keys := make([]*pb.LiteralNodeProto, 0)
	values := make([]*pb.LiteralNodeProto, 0)
	for {
		ok, err := i.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if p, err := ToProto(i.Key()); err == nil {
			keys = append(keys, p.GetLiteral())
		}
		if p, err := ToProto(i.Value()); err == nil {
			values = append(values, p.GetLiteral())
		} else {
			return p, err
		}
	}
	return &pb.NodeProto{
		Node: &pb.NodeProto_Literal{
			Literal: &pb.LiteralNodeProto{
				Value: &pb.LiteralNodeProto_CollectionValue{
					CollectionValue: &pb.CollectionProto{
						Keys:   keys,
						Values: values,
					},
				},
			},
		},
	}, nil
}
