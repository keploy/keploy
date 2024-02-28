package com.example.retailer.core;

public class Order {
    private String customer;
    private Double total;
    private Integer noOfItems;

    public Order(String customer, Double total, Integer noOfItems) {
        this.customer = customer;
        this.total = total;
        this.noOfItems = noOfItems;
    }

    public String getCustomer() {
        return customer;
    }

    public void setCustomer(String customer) {
        this.customer = customer;
    }

    public Double getTotal() {
        return total;
    }

    public void setTotal(Double total) {
        this.total = total;
    }

    public Integer getNoOfItems() {
        return noOfItems;
    }

    public void setNoOfItems(Integer noOfItems) {
        this.noOfItems = noOfItems;
    }
}
