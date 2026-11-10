"""SQLAlchemy fixture: declarative models and the operations against them."""

from sqlalchemy import Column, Integer, String, select
from sqlalchemy.orm import Session, declarative_base

Base = declarative_base()


class Order(Base):
    __tablename__ = "orders"

    id = Column(Integer, primary_key=True)
    merchant_id = Column("merchant", String)
    status = Column(String)


class Shipment(Base):
    __tablename__ = "shipments"

    id = Column(Integer, primary_key=True)


def list_orders(session: Session, merchant: str):
    return session.query(Order).filter(Order.merchant == merchant).all()


def create_order(session: Session, status: str):
    order = Order(status=status)
    session.add(order)
    return order


def purge_shipments(session: Session):
    session.query(Shipment).filter(Shipment.id < 100).delete()


def recent(session: Session):
    return session.execute(select(Order).limit(10))
