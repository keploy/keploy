package com.example.retailer.core;

import org.springframework.stereotype.Service;

@Service
public class RetailerService {

    public Item getItemDetails(String itemId) {
        return new Item("Apple", "iPhone", 1000.0);
    }

    public Order getOrderDetails(String orderId) {
        return new Order("John", 500.0, 5);
    }
}
