package com.example.product.core;

public class Item {
    private String brand;
    private String name;
    private Double price;

    public Item(){}

    public Item(String brand, String name, Double price) {
        this.brand = brand;
        this.name = name;
        this.price = price;
    }

    public String getBrand() {
        return brand;
    }

    public void setBrand(String brand) {
        this.brand = brand;
    }

    public String getName() {
        return name;
    }

    public void setName(String name) {
        this.name = name;
    }

    public Double getPrice() {
        return price;
    }
}

